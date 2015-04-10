package daemon

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/graph"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/broadcastwriter"
	"github.com/docker/docker/pkg/common"
	"github.com/docker/docker/pkg/graphdb"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/pkg/truncindex"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/trust"
	"github.com/docker/docker/volumes"
)

var (
	validContainerNameChars   = `[a-zA-Z0-9][a-zA-Z0-9_.-]`
	validContainerNamePattern = regexp.MustCompile(`^/?` + validContainerNameChars + `+$`)
)

type contStore struct {
	s map[string]*Container
	sync.Mutex
}

func (c *contStore) Add(id string, cont *Container) {
	c.Lock()
	c.s[id] = cont
	c.Unlock()
}

func (c *contStore) Get(id string) *Container {
	c.Lock()
	res := c.s[id]
	c.Unlock()
	return res
}

func (c *contStore) Delete(id string) {
	c.Lock()
	delete(c.s, id)
	c.Unlock()
}

func (c *contStore) List() []*Container {
	containers := new(History)
	c.Lock()
	for _, cont := range c.s {
		containers.Add(cont)
	}
	c.Unlock()
	containers.Sort()
	return *containers
}

type Daemon struct {
	ID             string
	repository     string
	sysInitPath    string
	containers     *contStore
	execCommands   *execStore
	graph          *graph.Graph
	repositories   *graph.TagStore
	idIndex        *truncindex.TruncIndex
	sysInfo        *sysinfo.SysInfo
	volumes        *volumes.Repository
	eng            *engine.Engine
	config         *Config
	containerGraph *graphdb.Database
	driver         graphdriver.Driver
	execDriver     execdriver.Driver
	trustStore     *trust.TrustStore
	statsCollector *statsCollector
}

// Install installs daemon capabilities to eng.
func (daemon *Daemon) Install(eng *engine.Engine) error {
	// FIXME: remove ImageDelete's dependency on Daemon, then move to graph/
	for name, method := range map[string]engine.Handler{
		"attach":            daemon.ContainerAttach,
		"commit":            daemon.ContainerCommit,
		"container_changes": daemon.ContainerChanges,
		"container_copy":    daemon.ContainerCopy,
		"container_rename":  daemon.ContainerRename,
		"container_inspect": daemon.ContainerInspect,
		"container_stats":   daemon.ContainerStats,
		"containers":        daemon.Containers,
		"create":            daemon.ContainerCreate,
		"rm":                daemon.ContainerRm,
		"export":            daemon.ContainerExport,
		"info":              daemon.CmdInfo,
		"kill":              daemon.ContainerKill,
		"logs":              daemon.ContainerLogs,
		"pause":             daemon.ContainerPause,
		"resize":            daemon.ContainerResize,
		"restart":           daemon.ContainerRestart,
		"start":             daemon.ContainerStart,
		"stop":              daemon.ContainerStop,
		"top":               daemon.ContainerTop,
		"unpause":           daemon.ContainerUnpause,
		"wait":              daemon.ContainerWait,
		"image_delete":      daemon.ImageDelete, // FIXME: see above
		"execCreate":        daemon.ContainerExecCreate,
		"execStart":         daemon.ContainerExecStart,
		"execResize":        daemon.ContainerExecResize,
		"execInspect":       daemon.ContainerExecInspect,
	} {
		if err := eng.Register(name, method); err != nil {
			return err
		}
	}
	if err := daemon.Repositories().Install(eng); err != nil {
		return err
	}
	if err := daemon.trustStore.Install(eng); err != nil {
		return err
	}
	// FIXME: this hack is necessary for legacy integration tests to access
	// the daemon object.
	eng.Hack_SetGlobalVar("httpapi.daemon", daemon)
	return nil
}

// Get looks for a container using the provided information, which could be
// one of the following inputs from the caller:
//  - A full container ID, which will exact match a container in daemon's list
//  - A container name, which will only exact match via the GetByName() function
//  - A partial container ID prefix (e.g. short ID) of any length that is
//    unique enough to only return a single container object
//  If none of these searches succeed, an error is returned
func (daemon *Daemon) Get(prefixOrName string) (*Container, error) {
	if containerByID := daemon.containers.Get(prefixOrName); containerByID != nil {
		// prefix is an exact match to a full container ID
		return containerByID, nil
	}

	// GetByName will match only an exact name provided; we ignore errors
	containerByName, _ := daemon.GetByName(prefixOrName)
	containerId, indexError := daemon.idIndex.Get(prefixOrName)

	if containerByName != nil {
		// prefix is an exact match to a full container Name
		return containerByName, nil
	}

	if containerId != "" {
		// prefix is a fuzzy match to a container ID
		return daemon.containers.Get(containerId), nil
	}
	return nil, indexError
}

// Exists returns a true if a container of the specified ID or name exists,
// false otherwise.
func (daemon *Daemon) Exists(id string) bool {
	c, _ := daemon.Get(id)
	return c != nil
}

func (daemon *Daemon) containerRoot(id string) string {
	return filepath.Join(daemon.repository, id)
}

// Load reads the contents of a container from disk
// This is typically done at startup.
func (daemon *Daemon) load(id string) (*Container, error) {
	container := &Container{
		root:         daemon.containerRoot(id),
		State:        NewState(),
		execCommands: newExecStore(),
	}
	if err := container.FromDisk(); err != nil {
		return nil, err
	}

	if container.ID != id {
		return container, fmt.Errorf("Container %s is stored at %s", container.ID, id)
	}

	container.readHostConfig()

	return container, nil
}

// Register makes a container object usable by the daemon as <container.ID>
// This is a wrapper for register
func (daemon *Daemon) Register(container *Container) error {
	return daemon.register(container, true)
}

// register makes a container object usable by the daemon as <container.ID>
func (daemon *Daemon) register(container *Container, updateSuffixarray bool) error {
	if container.daemon != nil || daemon.Exists(container.ID) {
		return fmt.Errorf("Container is already loaded")
	}
	if err := validateID(container.ID); err != nil {
		return err
	}
	if err := daemon.ensureName(container); err != nil {
		return err
	}

	container.daemon = daemon

	// Attach to stdout and stderr
	container.stderr = broadcastwriter.New()
	container.stdout = broadcastwriter.New()
	// Attach to stdin
	if container.Config.OpenStdin {
		container.stdin, container.stdinPipe = io.Pipe()
	} else {
		container.stdinPipe = ioutils.NopWriteCloser(ioutil.Discard) // Silently drop stdin
	}
	// done
	daemon.containers.Add(container.ID, container)

	// don't update the Suffixarray if we're starting up
	// we'll waste time if we update it for every container
	daemon.idIndex.Add(container.ID)

	container.registerVolumes()

	// FIXME: if the container is supposed to be running but is not, auto restart it?
	//        if so, then we need to restart monitor and init a new lock
	// If the container is supposed to be running, make sure of it
	if container.IsRunning() {
		log.Debugf("killing old running container %s", container.ID)

		existingPid := container.Pid
		container.SetStopped(&execdriver.ExitStatus{ExitCode: 0})

		// We only have to handle this for lxc because the other drivers will ensure that
		// no processes are left when docker dies
		if container.ExecDriver == "" || strings.Contains(container.ExecDriver, "lxc") {
			KillIfLxc(container.ID)
		} else {
			// use the current driver and ensure that the container is dead x.x
			cmd := &execdriver.Command{
				ID: container.ID,
			}
			var err error
			cmd.ProcessConfig.Process, err = os.FindProcess(existingPid)
			if err != nil {
				log.Debugf("cannot find existing process for %d", existingPid)
			}
			daemon.execDriver.Terminate(cmd)
		}

		if err := container.Unmount(); err != nil {
			log.Debugf("unmount error %s", err)
		}
		if err := container.ToDisk(); err != nil {
			log.Debugf("saving stopped state to disk %s", err)
		}

		info := daemon.execDriver.Info(container.ID)
		if !info.IsRunning() {
			log.Debugf("Container %s was supposed to be running but is not.", container.ID)

			log.Debugf("Marking as stopped")

			container.SetStopped(&execdriver.ExitStatus{ExitCode: -127})
			if err := container.ToDisk(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (daemon *Daemon) ensureName(container *Container) error {
	if container.Name == "" {
		name, err := daemon.generateNewName(container.ID)
		if err != nil {
			return err
		}
		container.Name = name

		if err := container.ToDisk(); err != nil {
			log.Debugf("Error saving container name %s", err)
		}
	}
	return nil
}

func (daemon *Daemon) LogToDisk(src *broadcastwriter.BroadcastWriter, dst, stream string) error {
	log, err := os.OpenFile(dst, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	src.AddWriter(log, stream)
	return nil
}

func (daemon *Daemon) restore() error {
	var (
		debug         = (os.Getenv("DEBUG") != "" || os.Getenv("TEST") != "")
		containers    = make(map[string]*Container)
		currentDriver = daemon.driver.String()
	)

	if !debug {
		log.Infof("Loading containers: start.")
	}
	dir, err := ioutil.ReadDir(daemon.repository)
	if err != nil {
		return err
	}

	for _, v := range dir {
		id := v.Name()
		container, err := daemon.load(id)
		if !debug {
			fmt.Print(".")
		}
		if err != nil {
			log.Errorf("Failed to load container %v: %v", id, err)
			continue
		}

		// Ignore the container if it does not support the current driver being used by the graph
		if (container.Driver == "" && currentDriver == "aufs") || container.Driver == currentDriver {
			log.Debugf("Loaded container %v", container.ID)

			containers[container.ID] = container
		} else {
			log.Debugf("Cannot load container %s because it was created with another graph driver.", container.ID)
		}
	}

	registeredContainers := []*Container{}

	if entities := daemon.containerGraph.List("/", -1); entities != nil {
		for _, p := range entities.Paths() {
			if !debug {
				fmt.Print(".")
			}

			e := entities[p]

			if container, ok := containers[e.ID()]; ok {
				if err := daemon.register(container, false); err != nil {
					log.Debugf("Failed to register container %s: %s", container.ID, err)
				}

				registeredContainers = append(registeredContainers, container)

				// delete from the map so that a new name is not automatically generated
				delete(containers, e.ID())
			}
		}
	}

	// Any containers that are left over do not exist in the graph
	for _, container := range containers {
		// Try to set the default name for a container if it exists prior to links
		container.Name, err = daemon.generateNewName(container.ID)
		if err != nil {
			log.Debugf("Setting default id - %s", err)
		}

		if err := daemon.register(container, false); err != nil {
			log.Debugf("Failed to register container %s: %s", container.ID, err)
		}

		registeredContainers = append(registeredContainers, container)
	}

	// check the restart policy on the containers and restart any container with
	// the restart policy of "always"
	if daemon.config.AutoRestart {
		log.Debugf("Restarting containers...")

		for _, container := range registeredContainers {
			if container.hostConfig.RestartPolicy.Name == "always" ||
				(container.hostConfig.RestartPolicy.Name == "on-failure" && container.ExitCode != 0) {
				log.Debugf("Starting container %s", container.ID)

				if err := container.Start(); err != nil {
					log.Debugf("Failed to start container %s: %s", container.ID, err)
				}
			}
		}
	}

	if !debug {
		fmt.Println()
		log.Infof("Loading containers: done.")
	}

	return nil
}

func (daemon *Daemon) checkDeprecatedExpose(config *runconfig.Config) bool {
	if config != nil {
		if config.PortSpecs != nil {
			for _, p := range config.PortSpecs {
				if strings.Contains(p, ":") {
					return true
				}
			}
		}
	}
	return false
}

func (daemon *Daemon) mergeAndVerifyConfig(config *runconfig.Config, img *image.Image) ([]string, error) {
	warnings := []string{}
	if (img != nil && daemon.checkDeprecatedExpose(img.Config)) || daemon.checkDeprecatedExpose(config) {
		warnings = append(warnings, "The mapping to public ports on your host via Dockerfile EXPOSE (host:port:port) has been deprecated. Use -p to publish the ports.")
	}
	if img != nil && img.Config != nil {
		if err := runconfig.Merge(config, img.Config); err != nil {
			return nil, err
		}
	}
	if len(config.Entrypoint) == 0 && len(config.Cmd) == 0 {
		return nil, fmt.Errorf("No command specified")
	}
	return warnings, nil
}

func (daemon *Daemon) generateIdAndName(name string) (string, string, error) {
	var (
		err error
		id  = common.GenerateRandomID()
	)

	if name == "" {
		if name, err = daemon.generateNewName(id); err != nil {
			return "", "", err
		}
		return id, name, nil
	}

	if name, err = daemon.reserveName(id, name); err != nil {
		return "", "", err
	}

	return id, name, nil
}

func (daemon *Daemon) reserveName(id, name string) (string, error) {
	if !validContainerNamePattern.MatchString(name) {
		return "", fmt.Errorf("Invalid container name (%s), only %s are allowed", name, validContainerNameChars)
	}

	if name[0] != '/' {
		name = "/" + name
	}

	if _, err := daemon.containerGraph.Set(name, id); err != nil {
		if !graphdb.IsNonUniqueNameError(err) {
			return "", err
		}

		conflictingContainer, err := daemon.GetByName(name)
		if err != nil {
			if strings.Contains(err.Error(), "Could not find entity") {
				return "", err
			}

			// Remove name and continue starting the container
			if err := daemon.containerGraph.Delete(name); err != nil {
				return "", err
			}
		} else {
			nameAsKnownByUser := strings.TrimPrefix(name, "/")
			return "", fmt.Errorf(
				"Conflict. The name %q is already in use by container %s. You have to delete (or rename) that container to be able to reuse that name.", nameAsKnownByUser,
				common.TruncateID(conflictingContainer.ID))
		}
	}
	return name, nil
}

func (daemon *Daemon) generateNewName(id string) (string, error) {
	var name string
	for i := 0; i < 6; i++ {
		name = namesgenerator.GetRandomName(i)
		if name[0] != '/' {
			name = "/" + name
		}

		if _, err := daemon.containerGraph.Set(name, id); err != nil {
			if !graphdb.IsNonUniqueNameError(err) {
				return "", err
			}
			continue
		}
		return name, nil
	}

	name = "/" + common.TruncateID(id)
	if _, err := daemon.containerGraph.Set(name, id); err != nil {
		return "", err
	}
	return name, nil
}

func (daemon *Daemon) generateHostname(id string, config *runconfig.Config) {
	// Generate default hostname
	// FIXME: the lxc template no longer needs to set a default hostname
	if config.Hostname == "" {
		config.Hostname = id[:12]
	}
}

func (daemon *Daemon) getEntrypointAndArgs(configEntrypoint, configCmd []string) (string, []string) {
	var (
		entrypoint string
		args       []string
	)
	if len(configEntrypoint) != 0 {
		entrypoint = configEntrypoint[0]
		args = append(configEntrypoint[1:], configCmd...)
	} else {
		entrypoint = configCmd[0]
		args = configCmd[1:]
	}
	return entrypoint, args
}

func (daemon *Daemon) newContainer(name string, config *runconfig.Config, imgID string) (*Container, error) {
	var (
		id  string
		err error
	)
	id, name, err = daemon.generateIdAndName(name)
	if err != nil {
		return nil, err
	}

	daemon.generateHostname(id, config)
	entrypoint, args := daemon.getEntrypointAndArgs(config.Entrypoint, config.Cmd)

	container := &Container{
		// FIXME: we should generate the ID here instead of receiving it as an argument
		ID:              id,
		Created:         time.Now().UTC(),
		Path:            entrypoint,
		Args:            args, //FIXME: de-duplicate from config
		Config:          config,
		hostConfig:      &runconfig.HostConfig{},
		ImageID:         imgID,
		NetworkSettings: &NetworkSettings{},
		Name:            name,
		Driver:          daemon.driver.String(),
		ExecDriver:      daemon.execDriver.Name(),
		State:           NewState(),
		execCommands:    newExecStore(),
	}
	container.root = daemon.containerRoot(container.ID)
	return container, err
}

func GetFullContainerName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("Container name cannot be empty")
	}
	if name[0] != '/' {
		name = "/" + name
	}
	return name, nil
}

func (daemon *Daemon) GetByName(name string) (*Container, error) {
	fullName, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	entity := daemon.containerGraph.Get(fullName)
	if entity == nil {
		return nil, fmt.Errorf("Could not find entity for %s", name)
	}
	e := daemon.containers.Get(entity.ID())
	if e == nil {
		return nil, fmt.Errorf("Could not find container for entity id %s", entity.ID())
	}
	return e, nil
}

func (daemon *Daemon) Children(name string) (map[string]*Container, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	children := make(map[string]*Container)

	err = daemon.containerGraph.Walk(name, func(p string, e *graphdb.Entity) error {
		c, err := daemon.Get(e.ID())
		if err != nil {
			return err
		}
		children[p] = c
		return nil
	}, 0)

	if err != nil {
		return nil, err
	}
	return children, nil
}

func (daemon *Daemon) Parents(name string) ([]string, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}

	return daemon.containerGraph.Parents(name)
}

func (daemon *Daemon) RegisterLink(parent, child *Container, alias string) error {
	fullName := filepath.Join(parent.Name, alias)
	if !daemon.containerGraph.Exists(fullName) {
		_, err := daemon.containerGraph.Set(fullName, child.ID)
		return err
	}
	return nil
}

func (daemon *Daemon) RegisterLinks(container *Container, hostConfig *runconfig.HostConfig) error {
	if hostConfig != nil && hostConfig.Links != nil {
		for _, l := range hostConfig.Links {
			parts, err := parsers.PartParser("name:alias", l)
			if err != nil {
				return err
			}
			child, err := daemon.Get(parts["name"])
			if err != nil {
				//An error from daemon.Get() means this name could not be found
				return fmt.Errorf("Could not get container for %s", parts["name"])
			}
			if child.hostConfig.NetworkMode.IsHost() {
				return runconfig.ErrConflictHostNetworkAndLinks
			}
			if err := daemon.RegisterLink(container, child, parts["alias"]); err != nil {
				return err
			}
		}

		// After we load all the links into the daemon
		// set them to nil on the hostconfig
		hostConfig.Links = nil
		if err := container.WriteHostConfig(); err != nil {
			return err
		}
	}
	return nil
}

// FIXME: harmonize with NewGraph()
func NewDaemon(config *Config, eng *engine.Engine) (*Daemon, error) {
	daemon, err := NewDaemonFromDirectory(config, eng)
	if err != nil {
		return nil, err
	}
	return daemon, nil
}

func (daemon *Daemon) shutdown() error {
	group := sync.WaitGroup{}
	log.Debugf("starting clean shutdown of all containers...")
	for _, container := range daemon.List() {
		c := container
		if c.IsRunning() {
			log.Debugf("stopping %s", c.ID)
			group.Add(1)

			go func() {
				defer group.Done()
				if err := c.KillSig(15); err != nil {
					log.Debugf("kill 15 error for %s - %s", c.ID, err)
				}
				c.WaitStop(-1 * time.Second)
				log.Debugf("container stopped %s", c.ID)
			}()
		}
	}
	group.Wait()

	return nil
}

func (daemon *Daemon) Mount(container *Container) error {
	dir, err := daemon.driver.Get(container.ID, container.GetMountLabel())
	if err != nil {
		return fmt.Errorf("Error getting container %s from driver %s: %s", container.ID, daemon.driver, err)
	}
	if container.basefs != dir {
		if container.basefs != "" && runtime.GOOS != "windows" {
			daemon.driver.Put(container.ID)
			return fmt.Errorf("Error: driver %s is returning inconsistent paths for container %s ('%s' then '%s')",
				daemon.driver, container.ID, container.basefs, dir)
		}
	}
	container.basefs = dir
	return nil
}

func (daemon *Daemon) Unmount(container *Container) error {
	daemon.driver.Put(container.ID)
	return nil
}

func (daemon *Daemon) Run(c *Container, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (execdriver.ExitStatus, error) {
	return daemon.execDriver.Run(c.command, pipes, startCallback)
}

func (daemon *Daemon) Pause(c *Container) error {
	if err := daemon.execDriver.Pause(c.command); err != nil {
		return err
	}
	c.SetPaused()
	return nil
}

func (daemon *Daemon) Unpause(c *Container) error {
	if err := daemon.execDriver.Unpause(c.command); err != nil {
		return err
	}
	c.SetUnpaused()
	return nil
}

func (daemon *Daemon) Kill(c *Container, sig int) error {
	return daemon.execDriver.Kill(c.command, sig)
}

func (daemon *Daemon) Stats(c *Container) (*execdriver.ResourceStats, error) {
	return daemon.execDriver.Stats(c.ID)
}

func (daemon *Daemon) SubscribeToContainerStats(name string) (chan interface{}, error) {
	c, err := daemon.Get(name)
	if err != nil {
		return nil, err
	}
	ch := daemon.statsCollector.collect(c)
	return ch, nil
}

func (daemon *Daemon) UnsubscribeToContainerStats(name string, ch chan interface{}) error {
	c, err := daemon.Get(name)
	if err != nil {
		return err
	}
	daemon.statsCollector.unsubscribe(c, ch)
	return nil
}

// Nuke kills all containers then removes all content
// from the content root, including images, volumes and
// container filesystems.
// Again: this will remove your entire docker daemon!
// FIXME: this is deprecated, and only used in legacy
// tests. Please remove.
func (daemon *Daemon) Nuke() error {
	var wg sync.WaitGroup
	for _, container := range daemon.List() {
		wg.Add(1)
		go func(c *Container) {
			c.Kill()
			wg.Done()
		}(container)
	}
	wg.Wait()

	return os.RemoveAll(daemon.config.Root)
}

// FIXME: this is a convenience function for integration tests
// which need direct access to daemon.graph.
// Once the tests switch to using engine and jobs, this method
// can go away.
func (daemon *Daemon) Graph() *graph.Graph {
	return daemon.graph
}

func (daemon *Daemon) Repositories() *graph.TagStore {
	return daemon.repositories
}

func (daemon *Daemon) Config() *Config {
	return daemon.config
}

func (daemon *Daemon) SystemConfig() *sysinfo.SysInfo {
	return daemon.sysInfo
}

func (daemon *Daemon) SystemInitPath() string {
	return daemon.sysInitPath
}

func (daemon *Daemon) GraphDriver() graphdriver.Driver {
	return daemon.driver
}

func (daemon *Daemon) ExecutionDriver() execdriver.Driver {
	return daemon.execDriver
}

func (daemon *Daemon) ContainerGraph() *graphdb.Database {
	return daemon.containerGraph
}

func (daemon *Daemon) ImageGetCached(imgID string, config *runconfig.Config) (*image.Image, error) {
	// Retrieve all images
	images, err := daemon.Graph().Map()
	if err != nil {
		return nil, err
	}

	// Store the tree in a map of map (map[parentId][childId])
	imageMap := make(map[string]map[string]struct{})
	for _, img := range images {
		if _, exists := imageMap[img.Parent]; !exists {
			imageMap[img.Parent] = make(map[string]struct{})
		}
		imageMap[img.Parent][img.ID] = struct{}{}
	}

	// Loop on the children of the given image and check the config
	var match *image.Image
	for elem := range imageMap[imgID] {
		img, ok := images[elem]
		if !ok {
			return nil, fmt.Errorf("unable to find image %q", elem)
		}
		if runconfig.Compare(&img.ContainerConfig, config) {
			if match == nil || match.Created.Before(img.Created) {
				match = img
			}
		}
	}
	return match, nil
}

package main

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/finboxio/go-rancher-metadata/metadata"
)

type runner struct {
	Config  *Config
	Client  metadata.Client
	Version string

	quitChan chan os.Signal
}

func NewRunner(conf *Config) (*runner, error) {
	u, _ := url.Parse(conf.MetadataUrl)
	u.Path = path.Join(u.Path, conf.MetadataVersion)

	log.Infof("Initializing Rancher Metadata client (version %s)", conf.MetadataVersion)

	client, err := metadata.NewClientAndWait(u.String())
	if err != nil {
		return nil, fmt.Errorf("Failed to initialize Rancher Metadata client: %v", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	return &runner{
		Config:   conf,
		Client:   client,
		Version:  "init",
		quitChan: c,
	}, nil
}

func (r *runner) Run() error {
	if r.Config.OneTime {
		log.Info("Processing all templates once.")
		return r.poll()
	}

	log.Infof("Polling Metadata with %d second interval", r.Config.Interval)
	ticker := time.NewTicker(time.Duration(r.Config.Interval) * time.Second)
	defer ticker.Stop()
	for {
		if err := r.poll(); err != nil {
			log.Error(err)
		}

		select {
		case <-ticker.C:
		case signal := <-r.quitChan:
			log.Info("Exit requested by signal: ", signal)
			return nil
		}
	}
}

func (r *runner) poll() error {
	log.Debug("Checking for metadata change")
	newVersion, err := r.Client.GetVersion()
	if err != nil {
		time.Sleep(time.Second * 2)
		return fmt.Errorf("Failed to get Metadata version: %v", err)
	}

	if r.Version == newVersion {
		log.Debug("No changes in Metadata")
		return nil
	}

	log.Debugf("Old version: %s, New Version: %s", r.Version, newVersion)

	r.Version = newVersion
	ctx, err := r.createContext()
	if err != nil {
		time.Sleep(time.Second * 2)
		return fmt.Errorf("Failed to create context from Rancher Metadata: %v", err)
	}

	tmplFuncs := newFuncMap(ctx)
	for _, tmpl := range r.Config.Templates {
		if err := r.processTemplate(tmplFuncs, tmpl); err != nil {
			return err
		}
	}

	if r.Config.OneTime {
		log.Info("All templates processed. Exiting.")
	} else {
		log.Info("All templates processed. Waiting for changes in Metadata...")
	}

	return nil
}

func (r *runner) processTemplate(funcs template.FuncMap, t Template) error {
	log.Debugf("Processing template %s for destination %s", t.Source, t.Dest)
	if _, err := os.Stat(t.Source); os.IsNotExist(err) {
		log.Fatalf("Template '%s' is missing", t.Source)
	}

	tmplBytes, err := ioutil.ReadFile(t.Source)
	if err != nil {
		log.Fatalf("Could not read template '%s': %v", t.Source, err)
	}

	name := filepath.Base(t.Source)
	newTemplate, err := template.New(name).Funcs(funcs).Parse(string(tmplBytes))
	if err != nil {
		log.Fatalf("Could not parse template '%s': %v", t.Source, err)
	}

	buf := new(bytes.Buffer)
	if err := newTemplate.Execute(buf, nil); err != nil {
		log.Fatalf("Could not render template: '%s': %v", t.Source, err)
	}

	content := buf.Bytes()

	if t.Dest == "" {
		log.Debug("No destination specified. Printing to StdOut")
		os.Stdout.Write(content)
		return nil
	}

	log.Debug("Checking whether content has changed")
	same, err := sameContent(content, t.Dest)
	if err != nil {
		return fmt.Errorf("Could not compare content for %s: %v", t.Dest, err)
	}

	if same {
		log.Debugf("Destination %s is up to date", t.Dest)
		return nil
	}

	log.Debug("Creating staging file")
	stagingFile, err := createStagingFile(content, t.Dest)
	if err != nil {
		return err
	}

	defer os.Remove(stagingFile)

	if t.CheckCmd != "" {
		if err := check(t.CheckCmd, stagingFile); err != nil {
			return fmt.Errorf("Check command failed: %v", err)
		}
	}

	log.Debugf("Writing destination")
	if err = copyStagingToDestination(stagingFile, t.Dest); err != nil {
		return fmt.Errorf("Could not write destination file %s: %v", t.Dest, err)
	}

	log.Info("Destination file %s has been updated", t.Dest)

	if t.NotifyCmd != "" {
		if err := notify(t.NotifyCmd, t.NotifyOutput); err != nil {
			return fmt.Errorf("Notify command failed: %v", err)
		}
	}

	return nil
}

func copyStagingToDestination(stagingPath, destPath string) error {
	err := os.Rename(stagingPath, destPath)
	if err == nil {
		return nil
	}

	if !strings.Contains(err.Error(), "device or resource busy") {
		return err
	}

	// A 'device busy' error could mean that the files live in
	// different mounts. Try to read the staging file and write
	// it's content to the destination file.
	log.Debugf("Failed to rename staging file: %v", err)

	content, err := ioutil.ReadFile(stagingPath)
	if err != nil {
		return err
	}

	sfi, err := os.Stat(stagingPath)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(destPath, content, sfi.Mode()); err != nil {
		return err
	}

	if os_stat, ok := sfi.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(destPath, int(os_stat.Uid), int(os_stat.Gid)); err != nil {
			return err
		}
	}

	return nil
}

func (r *runner) createContext() (*TemplateContext, error) {
	log.Debug("Fetching Metadata")

	metaServices, err := r.Client.GetServices()
	if err != nil {
		return nil, err
	}
	metaContainers, err := r.Client.GetContainers()
	if err != nil {
		return nil, err
	}
	metaHosts, err := r.Client.GetHosts()
	if err != nil {
		return nil, err
	}
	metaSelf, err := r.Client.GetSelfContainer()
	if err != nil {
		return nil, err
	}
	metaStacks, err := r.Client.GetStacks()
	if err != nil {
		return nil, err
	}

	self := Self{}

	stacks := make([]*Stack, 0)
	stackMap := make(map[string]*Stack)
	for _, s := range metaStacks {
		stack := Stack{
			Stack: 		s,
			Services: make([]*Service, 0),
		}
		stacks = append(stacks, &stack)
		stackMap[s.Name] = &stack

		if s.Name == metaSelf.StackName {
			self.Stack = &stack
		}
	}

	hosts := make([]*Host, 0)
	hostMap := make(map[string]*Host)
	for _, h := range metaHosts {
		host := Host{
			Host: 			h,
			Labels: 		LabelMap(h.Labels),
			Containers: make([]*Container, 0),
		}

		hosts = append(hosts, &host)
		hostMap[host.UUID] = &host

		if h.UUID == metaSelf.HostUUID {
			self.Host = &host
		}
	}

	services := make([]*Service, 0)
	serviceMap := make(map[string]*Service)
	sidekickParent := make(map[string]*Service)
	for _, s := range metaServices {
		s.StackUUID = stackMap[s.StackName].UUID

		stackServiceName := s.StackName + "." + s.Name
		service := Service{
			Service: 		s,
			Sidekicks: 	make([]*Service, 0),
			Containers: make([]*Container, 0),
			Ports:      parseServicePorts(s.Ports),
			Labels: 		LabelMap(s.Labels),
			Links: 			LabelMap(s.Links),
			Metadata: 	MetadataMap(s.Metadata),
			Stack: 			stackMap[s.StackName],
			Primary: 		s.Name == s.PrimaryServiceName,
			Sidekick: 	s.Name != s.PrimaryServiceName,
		}

		services = append(services, &service)
		serviceMap[stackServiceName] = &service

		for _, sk := range s.Sidekicks {
			sidekickServiceName := service.Stack.Name + "." + sk
			sidekickParent[sidekickServiceName] = &service
		}

		if service.Primary {
			service.Stack.Services = append(service.Stack.Services, &service)
		}

		if s.UUID == metaSelf.ServiceUUID {
			self.Service = &service
		}
	}

	for sk, service := range sidekickParent {
		service.Sidekicks = append(service.Sidekicks, serviceMap[sk])
	}

	containers := make([]*Container, 0)
	deploymentParent := make(map[string]*Container)
	for _, c := range metaContainers {
		stackServiceName := c.StackName + "." + c.ServiceName
		container := Container{
			Container: 	c,
			Ports: 			parseServicePorts(c.Ports),
			Labels: 		LabelMap(c.Labels),
			Links: 			LabelMap(c.Links),
			Primary: 		c.Labels["io.rancher.service.launch.config"] == "io.rancher.service.primary.launch.config",
			Sidekick: 	c.Labels["io.rancher.service.launch.config"] != "io.rancher.service.primary.launch.config",
			Service: 		serviceMap[stackServiceName],
			Host: 			hostMap[c.HostUUID],
			Sidekicks: 	make([]*Container, 0),
		}

		if container.Service != nil {
			container.Service.Containers = append(container.Service.Containers, &container)
		}

		container.Host.Containers = append(container.Host.Containers, &container)

		if container.Primary {
			deployment := container.Labels.GetValue("io.rancher.service.deployment.unit")
			deploymentParent[deployment] = &container
		}

		if c.UUID == metaSelf.UUID {
			self.Container = &container
		}

		containers = append(containers, &container)
	}

	for _, container := range containers {
		deployment := container.Labels.GetValue("io.rancher.service.deployment.unit")
		parent, hasParent := deploymentParent[deployment]
		if container.Sidekick && hasParent {
			container.Parent = parent
			container.Service.Parent = parent.Service
			parent.Sidekicks = append(parent.Sidekicks, container)
		}
	}

	log.Debugf("Finished building context")

	ctx := TemplateContext{
		Hosts:      hosts,
		Services:   services,
		Containers: containers,
		Stacks: 		stacks,
		Self:       &self,
	}

	return &ctx, nil
}

// converts Metadata.Service.Ports string slice to a ServicePort slice
func parseServicePorts(ports []string) []ServicePort {
	var ret []ServicePort
	for _, port := range ports {
		parts := strings.Split(port, ":")
		if len(parts) == 2 {
			public := parts[0]
			if parts_ := strings.Split(parts[1], "/"); len(parts_) == 2 {
				ret = append(ret, ServicePort{
					PublicPort:   public,
					InternalPort: parts_[0],
					Protocol:     parts_[1],
				})
				continue
			}
		} else if len(parts) == 3 {
			public := parts[1]
			if parts_ := strings.Split(parts[2], "/"); len(parts_) == 2 {
				ret = append(ret, ServicePort{
					BindAddress:  parts[0],
					PublicPort:   public,
					InternalPort: parts_[0],
					Protocol:     parts_[1],
				})
				continue
			}
		}
		log.Warnf("Unexpected format of service port: %s", port)
	}

	return ret
}

func check(command, filePath string) error {
	command = strings.Replace(command, "{{staging}}", filePath, -1)
	log.Debugf("Running check command '%s'", command)
	cmd := exec.Command("/bin/sh", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logCmdOutput(command, out)
		return err
	}

	log.Debugf("Check cmd output: %q", string(out))
	return nil
}

func notify(command string, verbose bool) error {
	log.Infof("Executing notify command '%s'", command)
	cmd := exec.Command("/bin/sh", "-c", command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logCmdOutput(command, out)
		return err
	}

	if verbose {
		logCmdOutput(command, out)
	}

	log.Debugf("Notify cmd output: %q", string(out))
	return nil
}

func logCmdOutput(command string, output []byte) {
	for _, line := range strings.Split(string(output), "\n") {
		if line != "" {
			log.Infof("[%s]: %q", command, line)
		}
	}
}

func sameContent(content []byte, filePath string) (bool, error) {
	fileMd5, err := computeFileMd5(filePath)
	if err != nil {
		return false, fmt.Errorf("Could not calculate checksum for %s: %v",
			filePath, err)
	}

	hash := md5.New()
	hash.Write(content)
	contentMd5 := fmt.Sprintf("%x", hash.Sum(nil))

	log.Debugf("Checksum content: %s, checksum file: %s",
		contentMd5, fileMd5)

	if fileMd5 == contentMd5 {
		return true, nil
	}

	return false, nil
}

func computeFileMd5(filePath string) (string, error) {
	if _, err := os.Stat(filePath); err != nil {
		return "", nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func createStagingFile(content []byte, destFile string) (string, error) {
	fp, err := ioutil.TempFile(filepath.Dir(destFile), "."+filepath.Base(destFile)+"-")
	if err != nil {
		return "", fmt.Errorf("Could not create staging file for %s: %v", destFile, err)
	}

	log.Debugf("Created staging file %s", fp.Name())

	onErr := func() {
		fp.Close()
		os.Remove(fp.Name())
	}

	if _, err := fp.Write(content); err != nil {
		onErr()
		return "", fmt.Errorf("Could not write staging file for %s: %v", destFile, err)
	}

	log.Debug("Copying file permissions and owner from destination")
	if stat, err := os.Stat(destFile); err == nil {
		if err := fp.Chmod(stat.Mode()); err != nil {
			onErr()
			return "", fmt.Errorf("Failed to copy permissions from %s: %v", destFile, err)
		}
		if os_stat, ok := stat.Sys().(*syscall.Stat_t); ok {
			if err := fp.Chown(int(os_stat.Uid), int(os_stat.Gid)); err != nil {
				onErr()
				return "", fmt.Errorf("Failed to copy ownership: %v", err)
			}
		}
	}

	fp.Close()
	return fp.Name(), nil
}

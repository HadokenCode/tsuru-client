// Copyright 2016 tsuru-client authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package installer

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tsuru/config"
	"github.com/tsuru/tsuru-client/tsuru/installer/dm"
	"github.com/tsuru/tsuru/cmd"
)

var (
	defaultTsuruInstallConfig = &TsuruInstallConfig{
		DockerMachineConfig: dm.DefaultDockerMachineConfig,
		ComponentsConfig:    NewInstallConfig(dm.DefaultDockerMachineConfig.Name),
		CoreHosts:           1,
		AppsHosts:           1,
		DedicatedAppsHosts:  false,
		CoreDriversOpts:     make(map[string][]interface{}),
	}
)

type TsuruInstallConfig struct {
	*dm.DockerMachineConfig
	*ComponentsConfig
	CoreHosts          int
	CoreDriversOpts    map[string][]interface{}
	AppsHosts          int
	DedicatedAppsHosts bool
	AppsDriversOpts    map[string][]interface{}
}

type Installer struct {
	outWriter io.Writer
	errWriter io.Writer
}

func (i *Installer) Install(config *TsuruInstallConfig, dockerMachine *dm.DockerMachine) (*Installation, error) {
	fmt.Fprintf(i.outWriter, "Running pre-install checks...\n")
	if errChecks := preInstallChecks(config); errChecks != nil {
		return nil, fmt.Errorf("pre-install checks failed: %s", errChecks)
	}
	config.CoreDriversOpts[config.DriverName+"-open-port"] = []interface{}{strconv.Itoa(defaultTsuruAPIPort)}
	coreMachines, err := ProvisionMachines(dockerMachine, config.CoreHosts, config.CoreDriversOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to provision components machines: %s", err)
	}
	cluster, err := NewSwarmCluster(coreMachines, len(coreMachines))
	if err != nil {
		return nil, fmt.Errorf("failed to setup swarm cluster: %s", err)
	}
	for _, component := range TsuruComponents {
		fmt.Fprintf(i.outWriter, "Installing %s\n", component.Name())
		errInstall := component.Install(cluster, config.ComponentsConfig)
		if errInstall != nil {
			return nil, fmt.Errorf("error installing %s: %s", component.Name(), errInstall)
		}
		fmt.Fprintf(i.outWriter, "%s successfully installed!\n", component.Name())
	}
	fmt.Fprintf(i.outWriter, "Bootstrapping Tsuru API...")
	registryAddr, registryPort := parseAddress(config.ComponentsConfig.ComponentAddress["registry"], "5000")
	bootstrapOpts := &BoostrapOptions{
		Login:        config.ComponentsConfig.RootUserEmail,
		Password:     config.ComponentsConfig.RootUserPassword,
		Target:       fmt.Sprintf("http://%s:%d", cluster.GetManager().IP, defaultTsuruAPIPort),
		TargetName:   config.ComponentsConfig.TargetName,
		RegistryAddr: fmt.Sprintf("%s:%s", registryAddr, registryPort),
		NodesParams:  config.AppsDriversOpts,
	}
	var installMachines []*dm.Machine
	if config.DriverName == "virtualbox" {
		appsMachines, errProv := ProvisionPool(dockerMachine, config, coreMachines)
		if errProv != nil {
			return nil, errProv
		}
		machineIndex := make(map[string]*dm.Machine)
		installMachines = append(coreMachines, appsMachines...)
		for _, m := range installMachines {
			machineIndex[m.Name] = m
		}
		var uniqueMachines []*dm.Machine
		for _, v := range machineIndex {
			uniqueMachines = append(uniqueMachines, v)
		}
		installMachines = uniqueMachines
		var nodesAddr []string
		for _, m := range appsMachines {
			nodesAddr = append(nodesAddr, m.GetPrivateAddress())
		}
		bootstrapOpts.NodesToRegister = nodesAddr
	} else {
		installMachines = coreMachines
		if config.DedicatedAppsHosts {
			bootstrapOpts.NodesToCreate = config.AppsHosts
		} else {
			var nodesAddr []string
			for _, m := range coreMachines {
				nodesAddr = append(nodesAddr, m.GetPrivateAddress())
			}
			if config.AppsHosts > config.CoreHosts {
				bootstrapOpts.NodesToCreate = config.AppsHosts - config.CoreHosts
				bootstrapOpts.NodesToRegister = nodesAddr
			} else {
				bootstrapOpts.NodesToRegister = nodesAddr[:config.AppsHosts]
			}
		}
	}
	bootstraper := &TsuruBoostraper{opts: bootstrapOpts}
	err = bootstraper.Do()
	if err != nil {
		return nil, fmt.Errorf("Error bootstrapping tsuru: %s", err)
	}
	fmt.Fprintf(i.outWriter, "Applying iptables workaround for docker 1.12...\n")
	for _, m := range coreMachines {
		_, err = m.RunSSHCommand("PATH=$PATH:/usr/sbin/:/usr/local/sbin; sudo iptables -D DOCKER-ISOLATION -i docker_gwbridge -o docker0 -j DROP")
		if err != nil {
			fmt.Fprintf(i.errWriter, "Failed to apply iptables rule: %s. Maybe it is not needed anymore?\n", err)
		}
		_, err = m.RunSSHCommand("PATH=$PATH:/usr/sbin/:/usr/local/sbin; sudo iptables -D DOCKER-ISOLATION -i docker0 -o docker_gwbridge -j DROP")
		if err != nil {
			fmt.Fprintf(i.errWriter, "Failed to apply iptables rule: %s. Maybe it is not needed anymore?\n", err)
		}
	}
	return &Installation{
		CoreCluster:     cluster,
		InstallMachines: installMachines,
		Components:      TsuruComponents,
	}, nil
}

func parseConfigFile(file string) (*TsuruInstallConfig, error) {
	installConfig := defaultTsuruInstallConfig
	if file == "" {
		return installConfig, nil
	}
	err := config.ReadConfigFile(file)
	if err != nil {
		return nil, err
	}
	driverName, err := config.GetString("driver:name")
	if err == nil {
		installConfig.DriverName = driverName
	}
	name, err := config.GetString("name")
	if err == nil {
		installConfig.Name = name
	}
	hub, err := config.GetString("docker-hub-mirror")
	if err == nil {
		installConfig.DockerHubMirror = hub
	}
	driverOpts := make(dm.DriverOpts)
	opts, _ := config.Get("driver:options")
	if opts != nil {
		for k, v := range opts.(map[interface{}]interface{}) {
			switch k := k.(type) {
			case string:
				driverOpts[k] = v
			}
		}
		installConfig.DriverOpts = driverOpts
	}
	caPath, err := config.GetString("ca-path")
	if err == nil {
		installConfig.CAPath = caPath
	}
	cHosts, err := config.GetInt("hosts:core:size")
	if err == nil {
		installConfig.CoreHosts = cHosts
	}
	pHosts, err := config.GetInt("hosts:apps:size")
	if err == nil {
		installConfig.AppsHosts = pHosts
	}
	dedicated, err := config.GetBool("hosts:apps:dedicated")
	if err == nil {
		installConfig.DedicatedAppsHosts = dedicated
	}
	opts, _ = config.Get("hosts:core:driver:options")
	if opts != nil {
		installConfig.CoreDriversOpts, err = parseDriverOptsSlice(opts)
		if err != nil {
			return nil, err
		}
	}
	opts, _ = config.Get("hosts:apps:driver:options")
	if opts != nil {
		installConfig.AppsDriversOpts, err = parseDriverOptsSlice(opts)
		if err != nil {
			return nil, err
		}
	}
	installConfig.ComponentsConfig = NewInstallConfig(installConfig.Name)
	installConfig.ComponentsConfig.IaaSConfig = map[string]interface{}{
		"dockermachine": map[string]interface{}{
			"ca-path": "/certs",
			"driver": map[string]interface{}{
				"name":    installConfig.DriverName,
				"options": map[string]interface{}(installConfig.DriverOpts),
			},
		},
	}
	return installConfig, nil
}

func preInstallChecks(config *TsuruInstallConfig) error {
	exists, err := cmd.CheckIfTargetLabelExists(config.Name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("tsuru target \"%s\" already exists", config.Name)
	}
	return nil
}

func ProvisionPool(p dm.MachineProvisioner, config *TsuruInstallConfig, hosts []*dm.Machine) ([]*dm.Machine, error) {
	if config.DedicatedAppsHosts {
		return ProvisionMachines(p, config.AppsHosts, config.AppsDriversOpts)
	}
	if config.AppsHosts > len(hosts) {
		poolMachines, err := ProvisionMachines(p, config.AppsHosts-len(hosts), config.AppsDriversOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to provision pool hosts: %s", err)
		}
		return append(poolMachines, hosts...), nil
	}
	return hosts[:config.AppsHosts], nil
}

func ProvisionMachines(p dm.MachineProvisioner, numMachines int, configs map[string][]interface{}) ([]*dm.Machine, error) {
	var machines []*dm.Machine
	for i := 0; i < numMachines; i++ {
		opts := make(dm.DriverOpts)
		for k, v := range configs {
			idx := i % len(v)
			opts[k] = v[idx]
		}
		m, err := p.ProvisionMachine(opts)
		if err != nil {
			return nil, fmt.Errorf("failed to provision machines: %s", err)
		}
		machines = append(machines, m)
	}
	return machines, nil
}

type Installation struct {
	CoreCluster     ServiceCluster
	InstallMachines []*dm.Machine
	Components      []TsuruComponent
}

func (i *Installation) Summary() string {
	summary := fmt.Sprintf(`--- Installation Overview ---
Core Hosts:
%s
Core Components:
%s
`, i.buildClusterTable().String(), i.buildComponentsTable().String())
	return summary
}

func (i *Installation) buildClusterTable() *cmd.Table {
	t := cmd.NewTable()
	t.Headers = cmd.Row{"IP", "State", "Manager"}
	t.LineSeparator = true
	nodes, err := i.CoreCluster.ClusterInfo()
	if err != nil {
		t.AddRow(cmd.Row{fmt.Sprintf("failed to retrieve cluster info: %s", err)})
	}
	for _, n := range nodes {
		t.AddRow(cmd.Row{n.IP, n.State, strconv.FormatBool(n.Manager)})
	}
	return t
}

func (i *Installation) buildComponentsTable() *cmd.Table {
	t := cmd.NewTable()
	t.Headers = cmd.Row{"Component", "Ports", "Replicas"}
	t.LineSeparator = true
	for _, component := range i.Components {
		info, err := component.Status(i.CoreCluster)
		if err != nil {
			t.AddRow(cmd.Row{component.Name(), "?", fmt.Sprintf("%s", err)})
			continue
		}
		row := cmd.Row{component.Name(),
			strings.Join(info.Ports, ","),
			strconv.Itoa(info.Replicas),
		}
		t.AddRow(row)
	}
	return t
}
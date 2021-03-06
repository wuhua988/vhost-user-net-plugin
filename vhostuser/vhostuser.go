// Copyright 2017 Intel Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
)

const defaultCNIDir = "/var/lib/cni/vhostuser"

type VhostConf struct {
	Vhostname string `json:"vhostname"`  // Vhost Port name
	VhostMac  string `json:"vhostmac"`   // Vhost port MAC address
	Ifname    string `json:"ifname"`     // Interface name
	IfMac     string `json:"ifmac"`      // Interface Mac address
	Vhosttool string `json:"vhost_tool"` // Scripts for configuration
}

type NetConf struct {
	types.NetConf
	VhostConf VhostConf `json:"vhost,omitempty"`
	CNIDir    string    `json:"cniDir"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// ExecCommand Execute shell commands and return the output.
func ExecCommand(cmd string, args []string) ([]byte, error) {
	return exec.Command(cmd, args...).Output()
}

func loadConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	if n.CNIDir == "" {
		n.CNIDir = defaultCNIDir
	}

	return n, nil
}

// saveVhostConf Save the rendered netconf for cmdDel
func saveVhostConf(conf *NetConf, ContainerID string, IfName string) error {
	fileName := fmt.Sprintf("%s-%s.json", ContainerID[:12], IfName)
	if vhostConfBytes, err := json.Marshal(conf.VhostConf); err == nil {
		sockDir := filepath.Join(conf.CNIDir, ContainerID)
		path := filepath.Join(sockDir, fileName)

		return ioutil.WriteFile(path, vhostConfBytes, 0644)
	} else {
		return fmt.Errorf("error serializing delegate netconf: %v", err)
	}
}

func (vc *VhostConf) loadVhostConf(conf *NetConf, ContainerID string, IfName string) error {
	fileName := fmt.Sprintf("%s-%s.json", ContainerID[:12], IfName)
	sockDir := filepath.Join(conf.CNIDir, ContainerID)
	path := filepath.Join(sockDir, fileName)

	if data, err := ioutil.ReadFile(path); err == nil {
		if err = json.Unmarshal(data, vc); err != nil {
			return fmt.Errorf("failed to parse VhostConf: %v", err)
		}
	} else {
		return fmt.Errorf("failed to read config: %v", err)
	}

	return nil
}

func createVhostPort(conf *NetConf, ContainerID string, IfName string) error {
	s := []string{ContainerID[:12], IfName}
	sockRef := strings.Join(s, "-")

	sockDir := filepath.Join(conf.CNIDir, ContainerID)
	if _, err := os.Stat(sockDir); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(sockDir, 0700); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	sockPath := filepath.Join(sockDir, sockRef)

	// vppctl create vhost socket /tmp/sock0 server
	cmd_args := []string{"create", sockPath}
	if output, err := ExecCommand(conf.VhostConf.Vhosttool, cmd_args); err == nil {
		vhostName := strings.Replace(string(output), "\n", "", -1)

		cmd_args = []string{"getmac", vhostName}
		if output, err := ExecCommand(conf.VhostConf.Vhosttool, cmd_args); err == nil {
			conf.VhostConf.VhostMac = strings.Replace(string(output), "\n", "", -1)
		}

		conf.VhostConf.Vhostname = vhostName
		conf.VhostConf.Ifname = IfName
		conf.VhostConf.IfMac = GenerateRandomMacAddress()
		return saveVhostConf(conf, ContainerID, IfName)
	}

	return nil
}

func destroyVhostPort(conf *NetConf, ContainerID string, IfName string) error {
	vc := &VhostConf{}
	if err := vc.loadVhostConf(conf, ContainerID, IfName); err != nil {
		return err
	}

	//vppctl delete vhost-user VirtualEthernet0/0/0
	cmd_args := []string{"delete", vc.Vhostname}
	if _, err := ExecCommand(conf.VhostConf.Vhosttool, cmd_args); err == nil {
		path := filepath.Join(conf.CNIDir, ContainerID)

		folder, err := os.Open(path)
		if err != nil {
			return err
		}
		defer folder.Close()

		fileBaseName := fmt.Sprintf("%s-%s", ContainerID[:12], IfName)
		filesForContainerID, err := folder.Readdirnames(0)
		if err != nil {
			return err
		}
		numDeletedFiles := 0

		// Remove files with matching container ID and IF name
		for _, fileName := range filesForContainerID {
			if match, _ := regexp.MatchString(fileBaseName + ".*", fileName); match == true {
				file := filepath.Join(path, fileName)
				if err = os.Remove(file); err != nil {
					return err
				}
				numDeletedFiles++
			}
		}
		// Remove folder for container ID if it's empty
		if numDeletedFiles == len(filesForContainerID) {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
	}

	return nil
}

const NET_CONFIG_TEMPLATE = `{
	"ipAddr": "%s/32",
	"macAddr": "%s",
	"gateway": "169.254.1.1",
	"gwMac": "%s"
}
`

func GenerateRandomMacAddress() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}

	// Set the local bit and make sure not MC address
	macAddr := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		(buf[0]|0x2)&0xfe, buf[1], buf[2],
		buf[3], buf[4], buf[5])
	return macAddr
}

// SetupContainerNetwork Write the configuration to file
func SetupContainerNetwork(conf *NetConf, ContainerID string, containerIP string, IfName string) {
	cmd_args := []string{"config", conf.VhostConf.Vhostname, containerIP, conf.VhostConf.IfMac}
	ExecCommand(conf.VhostConf.Vhosttool, cmd_args)

	// Write the configuration to file
	config := fmt.Sprintf(NET_CONFIG_TEMPLATE, containerIP, conf.VhostConf.IfMac, conf.VhostConf.VhostMac)
	fileName := fmt.Sprintf("%s-%s-ip4.conf", ContainerID[:12], IfName)
	sockDir := filepath.Join(conf.CNIDir, ContainerID)
	configFile := filepath.Join(sockDir, fileName)
	ioutil.WriteFile(configFile, []byte(config), 0644)
}

func cmdAdd(args *skel.CmdArgs) error {
	var result *types.Result
	var n *NetConf

	n, err := loadConf(args.StdinData)
	if err != nil {
		return result.Print()
	}

	createVhostPort(n, args.ContainerID, args.IfName)

	if n.IPAM.Type != "" {
		// run the IPAM plugin and get back the config to apply
		result, err = ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			return fmt.Errorf("failed to set up IPAM: %v", err)
		}
		if result.IP4 == nil {
			return errors.New("IPAM plugin returned missing IPv4 config")
		}

		ContainerIP := result.IP4.IP.IP.String()
		SetupContainerNetwork(n, args.ContainerID, ContainerIP, args.IfName)
	}

	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	if n, err := loadConf(args.StdinData); err == nil {
		if err = destroyVhostPort(n, args.ContainerID, args.IfName); err != nil {
			return err
		}

		if n.IPAM.Type != "" {
			return ipam.ExecDel(n.IPAM.Type, args.StdinData)
		}
		return nil
	} else {
		return err
	}
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel)
}


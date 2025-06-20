/*
Copyright 2019 The Kubernetes Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"k8s.io/klog/v2"
)

// https://github.com/aws/efs-utils/blob/v1.30.2/dist/efs-utils.conf
const (
	efsUtilsConfigTemplate = `
#
# Copyright 2017-2018 Amazon.com, Inc. and its affiliates. All Rights Reserved.
#
# Licensed under the MIT License. See the LICENSE accompanying this file
# for the specific language governing permissions and limitations under
# the License.
#

[DEFAULT]
logging_level = INFO
logging_max_bytes = 1048576
logging_file_count = 10
# mode for /var/run/efs and subdirectories in octal
state_file_dir_mode = 750

[mount]
dns_name_format = {az}.{fs_id}.efs.{region}.{dns_name_suffix}
dns_name_suffix = amazonaws.com
#The region of the file system when mounting from on-premises or cross region.
{{if .Region -}}
region = {{.Region -}}
{{else -}}
#region = us-east-1
{{- end}}
stunnel_debug_enabled = false
#Uncomment the below option to save all stunnel logs for a file system to the same file
#stunnel_logs_file = /var/log/amazon/efs/{fs_id}.stunnel.log
stunnel_cafile = /etc/amazon/efs/efs-utils.crt
{{if .CsiDriverVersion -}}
csi_driver_version={{.CsiDriverVersion}}
{{else -}}
{{- end}}

# Validate the certificate hostname on mount. This option is not supported by certain stunnel versions.
stunnel_check_cert_hostname = true

# Use OCSP to check certificate validity. This option is not supported by certain stunnel versions.
stunnel_check_cert_validity = false

# Enable FIPS mode. stunnel complains if FIPS is available and enabled system-wide, but not set here.
{{if .FipsEnabled -}}
fips_mode_enabled = {{.FipsEnabled -}}
{{else -}}
#fips_mode_enabled = false
{{- end}}

# Define the port range that the TLS tunnel will choose from
port_range_lower_bound = 20049
port_range_upper_bound = {{.PortRangeUpperBound}}

# Optimize read_ahead_kb for Linux 5.4+
optimize_readahead = true

# By default, we enable the feature to fallback to mount with mount target ip address when dns name cannot be resolved
fall_back_to_mount_target_ip_address_enabled = true

# By default, we use IMDSv2 to get the instance metadata, set this to true if you want to disable IMDSv2 usage
disable_fetch_ec2_metadata_token = false


[mount.cn-north-1]
dns_name_suffix = amazonaws.com.cn

[mount.cn-northwest-1]
dns_name_suffix = amazonaws.com.cn

[mount.us-iso-west-1]
dns_name_suffix = c2s.ic.gov
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount.us-iso-east-1]
dns_name_suffix = c2s.ic.gov
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount.us-isob-west-1]
dns_name_suffix = sc2s.sgov.gov
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount.us-isob-east-1]
dns_name_suffix = sc2s.sgov.gov
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount.us-isof-east-1]
dns_name_suffix = csp.hci.ic.gov
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount.us-isof-south-1]
dns_name_suffix = csp.hci.ic.gov
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount.eu-isoe-west-1]
dns_name_suffix = cloud.adc-e.uk
stunnel_cafile = /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem

[mount-watchdog]
enabled = true
poll_interval_sec = 1
unmount_count_for_consistency = 5
unmount_grace_period_sec = 30

# Set client auth/access point certificate renewal rate. Minimum value is 1 minute.
tls_cert_renewal_interval_min = 60

# Periodically check the health of stunnel to make sure the connection is fully established
stunnel_health_check_enabled = true
stunnel_health_check_interval_min = 5
stunnel_health_check_command_timeout_sec = 30

enable_version_check = false

[client-info] 
source={{.EfsClientSource}}

[cloudwatch-log]
# enabled = true
log_group_name = /aws/efs/utils

# Possible values are : 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1827, and 3653
# Comment this config to prevent log deletion
retention_in_days = 14
`

	efsUtilsConfigFileName = "efs-utils.conf"
	efsStateDir            = "/var/run/efs"
	csiDriverVersion       = "v2.1.8"
)

// Watchdog defines the interface for process monitoring and supervising
type Watchdog interface {
	// start starts the watch dog along with the process
	start() error

	// stop stops the watch dog along with the process
	stop()
}

// execWatchdog is a watch dog that monitors a process and restart it
// if it has crashed accidentally
type execWatchdog struct {
	// the command to be exec and monitored
	execCmd string
	// the command arguments
	execArg []string
	// the cmd that is running
	cmd *exec.Cmd
	// efs-utils config file path
	efsUtilsCfgPath string
	// efs-utils static files path
	efsUtilsStaticFilesPath string
	// stopCh indicates if it should be stopped
	stopCh chan struct{}

	mu sync.Mutex
}

type efsUtilsConfig struct {
	EfsClientSource     string
	Region              string
	FipsEnabled         string
	PortRangeUpperBound string
	CsiDriverVersion    string
}

func newExecWatchdog(efsUtilsCfgPath, efsUtilsStaticFilesPath, cmd string, arg ...string) Watchdog {
	return &execWatchdog{
		efsUtilsCfgPath:         efsUtilsCfgPath,
		efsUtilsStaticFilesPath: efsUtilsStaticFilesPath,
		execCmd:                 cmd,
		execArg:                 arg,
		stopCh:                  make(chan struct{}),
	}
}

func (w *execWatchdog) start() error {
	if err := w.setup(GetVersion().EfsClientSource); err != nil {
		return err
	}

	if err := w.removeLibwrapOption(efsStateDir); err != nil {
		klog.Warningf("Error removing libwrap option from state files: %v", err)
	}

	go w.runLoop(w.stopCh)

	return nil
}

func (w *execWatchdog) setup(efsClientSource string) error {
	if err := w.restoreStaticFiles(); err != nil {
		return err
	}

	if err := w.updateConfig(efsClientSource); err != nil {
		return err
	}
	return nil
}

/*
*
At image build time, static files installed by efs-utils in the config directory, i.e. CAs file, need
to be saved in another place so that the other stateful files created at runtime, i.e. private key for
client certificate, in the same config directory can be persisted to host with a host path volume.
Otherwise creating a host path volume for that directory will clean up everything inside at the first time.
Those static files need to be copied back to the config directory when the driver starts up.
*/
func (w *execWatchdog) restoreStaticFiles() error {
	return copyWithoutOverwriting(w.efsUtilsStaticFilesPath, w.efsUtilsCfgPath)
}

func copyWithoutOverwriting(srcDir, dstDir string) error {
	if _, err := os.Stat(dstDir); err != nil {
		return err
	}

	entries, err := ioutil.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(srcDir, entry.Name())
		dst := filepath.Join(dstDir, entry.Name())

		if _, err := os.Stat(dst); os.IsNotExist(err) {
			klog.Infof("Copying %s since it doesn't exist", dst)
			if err := copyFile(src, dst); err != nil {
				return err
			}
		} else if filepath.Base(src) == "efs-utils.crt" {
			klog.Infof("Copying %s ", dst)
			if err := copyFile(src, dst); err != nil {
				return err
			}
		} else {
			klog.Infof("Skip copying %s since it exists already", dst)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

func (w *execWatchdog) updateConfig(efsClientSource string) error {
	efsCfgTemplate := template.Must(template.New("efs-utils-config").Parse(efsUtilsConfigTemplate))
	f, err := os.Create(filepath.Join(w.efsUtilsCfgPath, efsUtilsConfigFileName))
	if err != nil {
		return fmt.Errorf("cannot create config file %s for efs-utils. Error: %v", w.efsUtilsCfgPath, err)
	}
	defer f.Close()
	// used on Fargate, IMDS queries suffice otherwise
	region := os.Getenv("AWS_DEFAULT_REGION")
	fipsEnabled := os.Getenv("FIPS_ENABLED")
	portRangeUpperBound := os.Getenv("PORT_RANGE_UPPER_BOUND")
	val, err := strconv.Atoi(portRangeUpperBound)
	if err != nil || val < 21049 {
		portRangeUpperBound = "21049"
	}
	efsCfg := efsUtilsConfig{
		EfsClientSource: efsClientSource, 
		Region: region, 
		FipsEnabled: fipsEnabled, 
		PortRangeUpperBound: portRangeUpperBound
		CsiDriverVersion: csiDriverVersion
	}
	if err = efsCfgTemplate.Execute(f, efsCfg); err != nil {
		return fmt.Errorf("cannot update config %s for efs-utils. Error: %v", w.efsUtilsCfgPath, err)
	}
	return nil
}

// stop kills the underlying process and stops the watchdog
func (w *execWatchdog) stop() {
	close(w.stopCh)

	w.mu.Lock()
	if w.cmd.Process != nil {
		p := w.cmd.Process
		err := p.Kill()
		if err != nil {
			klog.Errorf("Failed to kill process: %s", err)
		}
	}
	w.mu.Unlock()
}

// runLoop starts the monitoring loop
func (w *execWatchdog) runLoop(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			klog.Info("stopping...")
			break
		default:
			err := w.exec()
			if err != nil {
				klog.Errorf("Process %s exits %s", w.execCmd, err)
			}
		}
	}
}

func (w *execWatchdog) exec() error {
	cmd := exec.Command(w.execCmd, w.execArg...)
	cmd.Stdout = newInfoRedirect(w.execCmd)
	cmd.Stderr = newErrRedirect(w.execCmd)

	w.cmd = cmd

	w.mu.Lock()
	err := cmd.Start()
	if err != nil {
		return err
	}
	w.mu.Unlock()

	return cmd.Wait()
}

/*
If upgrading directly from older 1.x version of driver without application pod deployment,
users may have existing mounts using stunnel instead of efs-proxy.
If their stunnel state files have explicitly set the libwrap option as no,
the newer stunnel binaries will fail to run as this option is no longer supported in al23.
To avoid any errors, we check for this config and remove it directly on startup.
*/
func (w *execWatchdog) removeLibwrapOption(stateDir string) error {
	stunnelFiles, err := ioutil.ReadDir(stateDir)
	if err != nil {
		return fmt.Errorf("error reading directory %s: %v", efsStateDir, err)
	}

	for _, file := range stunnelFiles {
		if strings.HasPrefix(file.Name(), "stunnel-config.") {
			filePath := filepath.Join(stateDir, file.Name())
			if err := removeLibwrapFromFile(filePath); err != nil {
				klog.Warningf("Error proccessing stunnel file %s: %v", filePath, err)
			}
		}
	}
	return nil
}

func removeLibwrapFromFile(configFile string) error {
	stunnelConfigData, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to read stunnel config: %v", err)
	}

	newStunnelConfigData := strings.Replace(string(stunnelConfigData), "libwrap = no", "", -1)

	err = os.WriteFile(configFile, []byte(newStunnelConfigData), 0644)
	if err != nil {
		return fmt.Errorf("failed to update stunnel config: %v", err)
	}

	return nil
}

type logRedirect struct {
	processName string
	level       string
	logFunc     func(string, ...interface{})
}

func newInfoRedirect(name string) *logRedirect {
	return &logRedirect{
		processName: name,
		level:       "Info",
		logFunc:     klog.V(4).Infof,
	}
}

func newErrRedirect(name string) *logRedirect {
	return &logRedirect{
		processName: name,
		level:       "Error",
		logFunc:     klog.Errorf,
	}
}
func (l *logRedirect) Write(p []byte) (n int, err error) {
	msg := fmt.Sprintf("%s[%s]: %s", l.processName, l.level, string(p))
	l.logFunc("%s", msg)
	return len(msg), nil
}

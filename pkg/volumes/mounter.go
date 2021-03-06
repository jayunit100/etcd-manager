/*
Copyright 2016 The Kubernetes Authors.

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

package volumes

import (
	"fmt"
	"os"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/util/mount"
)

type VolumeMountController struct {
	mounted map[string]*Volume

	provider Volumes
}

func newVolumeMountController(provider Volumes) *VolumeMountController {
	c := &VolumeMountController{}
	c.mounted = make(map[string]*Volume)
	c.provider = provider
	return c
}

func (k *VolumeMountController) mountMasterVolumes() ([]*Volume, error) {
	// TODO: mount ephemeral volumes (particular on AWS)?

	// Mount master volumes
	attached, err := k.attachMasterVolumes()
	if err != nil {
		return nil, fmt.Errorf("unable to attach master volumes: %v", err)
	}

	for _, v := range attached {
		if len(k.mounted) > 0 {
			// We only attempt to mount a single volume
			break
		}

		existing := k.mounted[v.ProviderID]
		if existing != nil {
			continue
		}

		glog.V(2).Infof("Master volume %q is attached at %q", v.ProviderID, v.LocalDevice)

		mountpoint := "/mnt/" + v.MountName

		// On ContainerOS, we mount to /mnt/disks instead (/mnt is readonly)
		_, err := os.Stat(PathFor("/mnt/disks"))
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("error checking for /mnt/disks: %v", err)
			}
		} else {
			mountpoint = "/mnt/disks/" + v.MountName
		}

		glog.Infof("Doing safe-format-and-mount of %s to %s", v.LocalDevice, mountpoint)
		fstype := ""
		err = k.safeFormatAndMount(v, mountpoint, fstype)
		if err != nil {
			glog.Warningf("unable to mount master volume: %q", err)
			continue
		}

		glog.Infof("mounted master volume %q on %s", v.ProviderID, mountpoint)

		v.Mountpoint = PathFor(mountpoint)
		k.mounted[v.ProviderID] = v
	}

	var volumes []*Volume
	for _, v := range k.mounted {
		volumes = append(volumes, v)
	}
	return volumes, nil
}

func (k *VolumeMountController) safeFormatAndMount(volume *Volume, mountpoint string, fstype string) error {
	// Wait for the device to show up
	device := ""
	for {
		found, err := k.provider.FindMountedVolume(volume)
		if err != nil {
			return err
		}

		if found != "" {
			device = found
			break
		}

		glog.Infof("Waiting for volume %q to be mounted", volume.ProviderID)
		time.Sleep(1 * time.Second)
	}
	glog.Infof("Found volume %q mounted at device %q", volume.ProviderID, device)

	safeFormatAndMount := &mount.SafeFormatAndMount{}

	if Containerized {
		// Build mount & exec implementations that execute in the host namespaces
		safeFormatAndMount.Interface = mount.NewNsenterMounter()
		safeFormatAndMount.Exec = NewNsEnterExec()

		// Note that we don't use PathFor for operations going through safeFormatAndMount,
		// because NewNsenterMounter and NewNsEnterExec will operate in the host
	} else {
		safeFormatAndMount.Interface = mount.New("")
		safeFormatAndMount.Exec = mount.NewOsExec()
	}

	// Check if it is already mounted
	// TODO: can we now use IsLikelyNotMountPoint or IsMountPointMatch instead here
	mounts, err := safeFormatAndMount.List()
	if err != nil {
		return fmt.Errorf("error listing existing mounts: %v", err)
	}

	var existing []*mount.MountPoint
	for i := range mounts {
		m := &mounts[i]
		glog.V(8).Infof("found existing mount: %v", m)
		// Note: when containerized, we still list mounts in the host, so we don't need to call PathFor(mountpoint)
		if m.Path == mountpoint {
			existing = append(existing, m)
		}
	}

	// Mount only if isn't mounted already
	if len(existing) == 0 {
		options := []string{}

		glog.Infof("Creating mount directory %q", PathFor(mountpoint))
		if err := os.MkdirAll(PathFor(mountpoint), 0750); err != nil {
			return err
		}

		glog.Infof("Mounting device %q on %q", device, mountpoint)

		err = safeFormatAndMount.FormatAndMount(device, mountpoint, fstype, options)
		if err != nil {
			return fmt.Errorf("error formatting and mounting disk %q on %q: %v", device, mountpoint, err)
		}
	} else {
		glog.Infof("Device already mounted on %q, verifying it is our device", mountpoint)

		if len(existing) != 1 {
			glog.Infof("Existing mounts unexpected")

			for i := range mounts {
				m := &mounts[i]
				glog.Infof("%s\t%s", m.Device, m.Path)
			}

			return fmt.Errorf("found multiple existing mounts of %q at %q", device, mountpoint)
		} else {
			glog.Infof("Found existing mount of %q at %q", device, mountpoint)
		}
	}

	// If we're containerized we also want to mount the device (again) into our container
	// We could also do this with mount propagation, but this is simple
	if Containerized {
		source := PathFor(device)
		target := PathFor(mountpoint)
		options := []string{}

		mounter := mount.New("")

		mountedDevice, _, err := mount.GetDeviceNameFromMount(mounter, target)
		if err != nil {
			return fmt.Errorf("error checking for mounts of %s inside container: %v", target, err)
		}

		if mountedDevice != "" {
			// We check that it is the correct device.  We also tolerate /dev/X as well as /root/dev/X
			if mountedDevice != source && mountedDevice != device {
				return fmt.Errorf("device already mounted at %s, but is %s and we want %s or %s", target, mountedDevice, source, device)
			}
		} else {
			glog.Infof("mounting inside container: %s -> %s", source, target)
			if err := mounter.Mount(source, target, fstype, options); err != nil {
				return fmt.Errorf("error mounting %s inside container at %s: %v", source, target, err)
			}
		}
	}

	return nil
}

func (k *VolumeMountController) attachMasterVolumes() ([]*Volume, error) {
	volumes, err := k.provider.FindVolumes()
	if err != nil {
		return nil, err
	}

	var tryAttach []*Volume
	var attached []*Volume
	for _, v := range volumes {
		if v.AttachedTo == "" {
			tryAttach = append(tryAttach, v)
		}
		if v.LocalDevice != "" {
			attached = append(attached, v)
		}
	}

	if len(tryAttach) == 0 {
		return attached, nil
	}

	// Actually attempt the mounting
	for _, v := range tryAttach {
		if len(attached) > 0 {
			// We only attempt to mount a single volume
			break
		}

		glog.V(2).Infof("Trying to mount master volume: %q", v.ProviderID)

		err := k.provider.AttachVolume(v)
		if err != nil {
			// We are racing with other instances here; this can happen
			glog.Warningf("Error attaching volume %q: %v", v.ProviderID, err)
		} else {
			if v.LocalDevice == "" {
				glog.Fatalf("AttachVolume did not set LocalDevice")
			}
			attached = append(attached, v)
		}
	}

	glog.V(2).Infof("Currently attached volumes: %v", attached)
	return attached, nil
}

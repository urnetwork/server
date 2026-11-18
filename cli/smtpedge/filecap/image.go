package filecap

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
	"time"
)

// The PAX record prefix a tar entry carries an extended attribute under, as containerd and moby write
// security.capability into an image's layers and into docker cp's archive.
const paxXattrPrefix = "SCHILY.xattr."

// What the release check runs and holds a published image to.
type ImageCheckSettings struct {
	// the docker command line's program
	DockerCommand string
	// docker create's --pull: always, so the check reads what the registry serves under the tag, as a host pulls it
	Pull string
	// each platform the image is published for, every one checked; server/cli/smtpedge's Makefile builds these
	Platforms []string
	// the binary's path in the image
	BinaryPath string
	// how long one docker command may run before the check fails; create pulls the platform's image
	CommandTimeout time.Duration
	// once a command is past its bound and killed, how long its output may still drain before its pipes close,
	// so a child it left holding them cannot hold the check
	CommandWaitDelay time.Duration
}

// The release check's settings: both published platforms, smtpedge's binary, and a pull from the registry.
func DefaultImageCheckSettings() *ImageCheckSettings {
	return &ImageCheckSettings{
		DockerCommand:    "docker",
		Pull:             "always",
		Platforms:        []string{"linux/amd64", "linux/arm64"},
		BinaryPath:       "/usr/local/sbin/bringyour-smtpedge",
		CommandTimeout:   10 * time.Minute,
		CommandWaitDelay: 5 * time.Second,
	}
}

// Reads the binary's attribute out of each of the image's platforms, as the host's docker unpacks it, and refuses
// the image unless every one is NetAdminFileCapability's, byte for byte (CheckNetAdmin). For each platform it
// creates a container that never starts, copies the binary out as docker cp's tar, whose entry carries the
// attribute as a PAX record, and removes the container on every path. The answer has one line per platform
// checked, the platform and what getcap would print.
func CheckImage(ctx context.Context, image string, settings *ImageCheckSettings) ([]string, error) {
	if len(settings.Platforms) == 0 {
		return nil, errors.New("no platform to check")
	}
	if !path.IsAbs(settings.BinaryPath) {
		return nil, fmt.Errorf("binary path %q is not absolute", settings.BinaryPath)
	}
	// one docker command under the command bound, its stderr kept for the error
	docker := func(ctx context.Context, stdout io.Writer, args ...string) error {
		commandCtx, cancel := context.WithTimeout(ctx, settings.CommandTimeout)
		defer cancel()
		command := exec.CommandContext(commandCtx, settings.DockerCommand, args...)
		command.WaitDelay = settings.CommandWaitDelay
		var stderr bytes.Buffer
		command.Stdout = stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}

	lines := []string{}
	for _, platform := range settings.Platforms {
		value, err := func() (value []byte, returnErr error) {
			var created bytes.Buffer
			if err := docker(ctx, &created, "create", "--quiet", "--pull", settings.Pull, "--platform", platform, image); err != nil {
				return nil, err
			}
			// the id is create's last line
			createdLines := strings.Split(strings.TrimSpace(created.String()), "\n")
			containerId := strings.TrimSpace(createdLines[len(createdLines)-1])
			if containerId == "" || strings.Trim(containerId, "0123456789abcdef") != "" {
				return nil, fmt.Errorf("docker create answered %q, not a container id", containerId)
			}
			defer func() {
				// the container goes even when the check's context has ended, and a check that cannot remove it
				// fails, unless it failed already
				if err := docker(context.WithoutCancel(ctx), io.Discard, "rm", containerId); err != nil && returnErr == nil {
					value, returnErr = nil, fmt.Errorf("remove the check's container: %w", err)
				}
			}()
			// docker cp's archive holds the one file, which the check reads whole
			var archive bytes.Buffer
			if err := docker(ctx, &archive, "cp", containerId+":"+settings.BinaryPath, "-"); err != nil {
				return nil, err
			}
			return ReadTarFileCapability(&archive, path.Base(settings.BinaryPath))
		}()
		if err != nil {
			return lines, fmt.Errorf("%s %s: %w", platform, settings.BinaryPath, err)
		}
		if err := CheckNetAdmin(value); err != nil {
			return lines, fmt.Errorf("%s %s: %w", platform, settings.BinaryPath, err)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", platform, settings.BinaryPath, NetAdminFileCapability()))
	}
	return lines, nil
}

// Finds the regular file named name in a tar stream and answers its security.capability PAX record's bytes; an
// entry that holds none answers ErrNoFileCapability.
func ReadTarFileCapability(reader io.Reader, name string) ([]byte, error) {
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the archive holds no file %s", name)
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || path.Clean(header.Name) != name {
			continue
		}
		value, ok := header.PAXRecords[paxXattrPrefix+AttributeName]
		if !ok {
			return nil, ErrNoFileCapability
		}
		return []byte(value), nil
	}
}

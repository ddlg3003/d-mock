package stack

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	cliOnce sync.Once
	cliName string
)

// ContainerCLI is the container engine command: $DMOCK_CONTAINER_CLI, else docker, else podman
// (podman takes the same arguments for everything d-mock does).
func ContainerCLI() string {
	cliOnce.Do(func() {
		if v := os.Getenv("DMOCK_CONTAINER_CLI"); v != "" {
			cliName = v
			return
		}
		for _, c := range []string{"docker", "podman"} {
			if _, err := exec.LookPath(c); err == nil {
				cliName = c
				return
			}
		}
		cliName = "docker"
	})
	return cliName
}

func docker(args ...string) (string, error) {
	cmd := exec.Command(ContainerCLI(), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", ContainerCLI(), strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ContainerLogs returns the last lines of a dep container's log.
func ContainerLogs(project, dep string, lines int) (string, error) {
	cmd := exec.Command(ContainerCLI(), "logs", "--tail", fmt.Sprint(lines), containerName(project, dep))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s logs: %w: %s", ContainerCLI(), err, out)
	}
	return string(out), nil
}

func containerRunning(name string) bool {
	out, err := docker("inspect", "-f", "{{.State.Running}}", name)
	return err == nil && out == "true"
}

func containerName(project, name string) string { return "dmock-" + project + "-" + name }

// runContainer (re)creates a detached container labelled with the project.
func runContainer(project, name, image string, ports []string, env map[string]string, volumes []string, args []string) (string, error) {
	cname := containerName(project, name)
	_, _ = docker("rm", "-f", cname)
	a := []string{"run", "-d", "--name", cname, "--label", "dmock.project=" + project}
	for _, p := range ports {
		a = append(a, "-p", p)
	}
	for _, k := range sortedKeys(env) {
		a = append(a, "-e", k+"="+env[k])
	}
	for _, v := range volumes {
		a = append(a, "-v", v)
	}
	a = append(a, image)
	a = append(a, args...)
	if _, err := docker(a...); err != nil {
		return "", err
	}
	return cname, nil
}

func removeContainer(name string) error {
	_, err := docker("rm", "-f", name)
	return err
}

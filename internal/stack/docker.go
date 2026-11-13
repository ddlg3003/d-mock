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

// HostAlias is the name a container uses to reach the host machine (mocks, local deps).
func HostAlias() string {
	if strings.Contains(ContainerCLI(), "podman") {
		return "host.containers.internal"
	}
	return "host.docker.internal"
}

// ContainerInfo is what `inspect` tells about a container.
type ContainerInfo struct {
	Running   bool
	OOMKilled bool
	ExitCode  int
	Exists    bool
}

func inspectContainer(name string) ContainerInfo {
	out, err := docker("inspect", "-f", "{{.State.Running}} {{.State.OOMKilled}} {{.State.ExitCode}}", name)
	if err != nil {
		return ContainerInfo{}
	}
	f := strings.Fields(out)
	if len(f) != 3 {
		return ContainerInfo{}
	}
	var code int
	fmt.Sscan(f[2], &code)
	return ContainerInfo{Exists: true, Running: f[0] == "true", OOMKilled: f[1] == "true", ExitCode: code}
}

// ContainerStats is a one-shot resource sample of a running container.
type ContainerStats struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemBytes   int64   `json:"mem_bytes"`
	MemLimit   int64   `json:"mem_limit_bytes"`
	MemPercent float64 `json:"mem_percent"`
}

func statsContainer(name string) (ContainerStats, error) {
	// {{.CPUPerc}} "12.34%", {{.MemUsage}} "100MiB / 512MiB", {{.MemPerc}} "19.53%"
	out, err := docker("stats", "--no-stream", "--format", "{{.CPUPerc}}|{{.MemUsage}}|{{.MemPerc}}", name)
	if err != nil {
		return ContainerStats{}, err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 3 {
		return ContainerStats{}, fmt.Errorf("unexpected stats output %q", out)
	}
	var st ContainerStats
	st.CPUPercent = parsePercent(parts[0])
	st.MemPercent = parsePercent(parts[2])
	if used, limit, ok := strings.Cut(parts[1], "/"); ok {
		st.MemBytes = parseBytes(used)
		st.MemLimit = parseBytes(limit)
	}
	return st, nil
}

func parsePercent(s string) float64 {
	var v float64
	fmt.Sscan(strings.TrimSuffix(strings.TrimSpace(s), "%"), &v)
	return v
}

// parseBytes reads docker/podman sizes like "12.5MiB", "1.2GB", "512kB", "100B".
func parseBytes(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			var v float64
			fmt.Sscan(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), &v)
			return int64(v * u.mult)
		}
	}
	var v float64
	fmt.Sscan(s, &v)
	return int64(v)
}

func buildImage(tag, context, dockerfile string, args map[string]string, logPath string) error {
	a := []string{"build", "-t", tag}
	if dockerfile != "" {
		a = append(a, "-f", dockerfile)
	}
	for _, k := range sortedKeys(args) {
		a = append(a, "--build-arg", k+"="+args[k])
	}
	a = append(a, context)
	cmd := exec.Command(ContainerCLI(), a...)
	out, err := cmd.CombinedOutput()
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		fmt.Fprintf(f, "\n=== d-mock build %s\n%s", tag, out)
		f.Close()
	}
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		return fmt.Errorf("%s build %s: %w\n%s", ContainerCLI(), tag, err, strings.Join(lines, "\n"))
	}
	return nil
}

// runServiceContainer starts a service container with resource limits and host access.
func runServiceContainer(project, name, image string, c containerOpts) (string, error) {
	cname := containerName(project, name)
	_, _ = docker("rm", "-f", cname)
	a := []string{"run", "-d", "--name", cname, "--label", "dmock.project=" + project, "--label", "dmock.kind=service"}
	if HostAlias() == "host.docker.internal" {
		a = append(a, "--add-host", "host.docker.internal:host-gateway") // Linux docker needs it; harmless elsewhere
	}
	if c.CPUs != "" {
		a = append(a, "--cpus", c.CPUs)
	}
	if c.Memory != "" {
		a = append(a, "--memory", c.Memory, "--memory-swap", c.Memory) // no swap: hitting the limit means OOM kill, like k8s
	}
	for _, p := range c.Ports {
		a = append(a, "-p", p)
	}
	for _, k := range sortedKeys(c.Env) {
		a = append(a, "-e", k+"="+c.Env[k])
	}
	for _, m := range c.Mounts {
		a = append(a, "-v", m)
	}
	a = append(a, image)
	a = append(a, c.Args...)
	if _, err := docker(a...); err != nil {
		return "", err
	}
	return cname, nil
}

type containerOpts struct {
	CPUs, Memory string
	Ports        []string
	Env          map[string]string
	Mounts       []string
	Args         []string
}

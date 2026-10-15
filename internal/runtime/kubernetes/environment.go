package kubernetes

// sandboxCommand removes kubelet's API Service variables before the target
// process starts. execve (rather than shell unset) also gives the target a clean
// initial /proc/self/environ. Keep the Pod's empty overrides: enableServiceLinks
// does not suppress the default Kubernetes Service, and raw CRI exec still
// receives the configured container environment.
//
// /usr/bin/env is provided by the Debian-based sandbox runtime. It execs the
// target in place, preserving PID 1, argv, stdin, signals and exit status.
func sandboxCommand(command []string) []string {
	variables := sandboxKubernetesEnv()
	argv := make([]string, 0, 2+2*len(variables)+len(command))
	argv = append(argv, "/usr/bin/env")
	for _, variable := range variables {
		argv = append(argv, "-u", variable.Name)
	}
	argv = append(argv, "--")
	return append(argv, command...)
}

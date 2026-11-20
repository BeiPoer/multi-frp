// frpbuild runs Go commands with the server-side FRP work connection patch.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const frpModule = "github.com/fatedier/frp"
const frpVersion = "v0.72.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: go run ./tools/frpbuild <build|test|run> [go arguments]")
	}
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	download := exec.Command(goTool, "mod", "download", "-json", frpModule)
	download.Stderr = os.Stderr
	output, err := download.Output()
	if err != nil {
		return fmt.Errorf("download %s: %w\n%s", frpModule, err, output)
	}
	var module struct{ Version, Dir string }
	if err := json.Unmarshal(output, &module); err != nil {
		return fmt.Errorf("read downloaded FRP module: %w", err)
	}
	if module.Version != frpVersion || module.Dir == "" {
		return fmt.Errorf("FRP work connection patch requires %s, got %q in %q", frpVersion, module.Version, module.Dir)
	}
	source, err := os.ReadFile(filepath.Join(module.Dir, "server", "service.go"))
	if err != nil {
		return err
	}
	patched, err := patchService(string(source))
	if err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp("", "multifrp-build-")
	if err != nil {
		return err
	}
	localFRP := filepath.Join(buildDir, "frp")
	if err := os.CopyFS(localFRP, os.DirFS(module.Dir)); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(localFRP, "server", "service.go"), []byte(patched), 0600); err != nil {
		return err
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(buildDir, name), data, 0600); err != nil {
			return err
		}
	}
	modFlag := "-modfile=" + filepath.Join(buildDir, "go.mod")
	edit := exec.Command(goTool, "mod", "edit", modFlag, "-replace="+frpModule+"="+localFRP)
	edit.Stderr = os.Stderr
	if err := edit.Run(); err != nil {
		return err
	}
	command := exec.Command(goTool, append([]string{args[0], modFlag}, args[1:]...)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func patchService(source string) (string, error) {
	edits := []struct{ old, new string }{
		{
			"\t\t\tacceptedConn.clientHelloPresent,\n\t\t); err != nil {",
			"\t\t\tacceptedConn.clientHelloPresent,\n\t\t\tsession,\n\t\t); err != nil {",
		},
		{
			"\tworkClientHelloPresent bool,\n) error {",
			"\tworkClientHelloPresent bool,\n\tworkSession io.Closer,\n) error {",
		},
		{
			"\t// server plugin hook\n\tcontent := &plugin.NewWorkConnContent{",
			"\tif err := RequireWorkConnLoginSession(ctl.sessionCtx.Session, workSession); err != nil {\n" +
				"\t\treturn err\n\t}\n\t// server plugin hook\n\tcontent := &plugin.NewWorkConnContent{",
		},
		{
			"// RegisterWorkConn register a new work connection to control and proxies need it.",
			`// RequireWorkConnLoginSession requires the work stream to share the authenticated transport session.
func RequireWorkConnLoginSession(loginSession, workSession io.Closer) error {
	if workSession == nil || workSession != loginSession {
		return fmt.Errorf("work connection must use the authenticated transport session")
	}
	return nil
}

// RegisterWorkConn register a new work connection to control and proxies need it.`,
		},
	}
	for _, edit := range edits {
		if strings.Count(source, edit.old) != 1 {
			return "", fmt.Errorf("FRP work connection patch could not uniquely match %q", edit.old)
		}
		source = strings.Replace(source, edit.old, edit.new, 1)
	}
	return source, nil
}

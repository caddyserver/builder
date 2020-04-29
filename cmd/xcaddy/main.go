// Copyright 2020 Matthew Holt
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
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/caddyserver/xcaddy"
)

var caddyVersion = os.Getenv("CADDY_VERSION")

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go trapSignals(ctx, cancel)

	if len(os.Args) > 1 && os.Args[1] == "build" {
		if err := runBuild(ctx, os.Args[2:]); err != nil {
			log.Fatalf("[ERROR] %v", err)
		}
		return
	}

	if err := runDev(ctx, os.Args[1:]); err != nil {
		log.Fatalf("[ERROR] %v", err)
	}
}

func runBuild(ctx context.Context, args []string) error {
	// parse the command line args... rather primitively
	var cgoAllowed bool
	var argCaddyVersion, output string
	var plugins []xcaddy.Dependency
	var replacements []xcaddy.Replace
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--with":
			if i == len(args)-1 {
				return fmt.Errorf("expected value after --with flag")
			}
			i++
			mod, ver, repl, err := splitWith(args[i])
			if err != nil {
				return err
			}
			plugins = append(plugins, xcaddy.Dependency{
				ModulePath: mod,
				Version:    ver,
			})
			if repl != "" {
				replacements = append(replacements, xcaddy.Replace{
					Old: mod,
					New: repl,
				})
			}

		case "--enable-cgo":
			cgoAllowed = true

		case "--output":
			if i == len(args)-1 {
				return fmt.Errorf("expected value after --output flag")
			}
			i++
			output = args[i]

		default:
			if argCaddyVersion != "" {
				return fmt.Errorf("missing flag; caddy version already set at %s", argCaddyVersion)
			}
			argCaddyVersion = args[i]
		}
	}

	// prefer caddy version from command line argument over env var
	if argCaddyVersion != "" {
		caddyVersion = argCaddyVersion
	}

	// ensure an output file is always specified
	if output == "" {
		if runtime.GOOS == "windows" {
			output = "caddy.exe"
		} else {
			output = "caddy"
		}
	}

	if cgoAllowed {
		output = "CGO_ENABLED=1 " + output
	}

	// perform the build
	builder := xcaddy.Builder{
		CaddyVersion: caddyVersion,
		Plugins:      plugins,
		Replacements: replacements,
	}
	err := builder.Build(ctx, output)
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	// prove the build is working by printing the version
	if !filepath.IsAbs(output) {
		output = "." + string(filepath.Separator) + output
	}
	fmt.Println()
	fmt.Printf("%s version\n", output)
	cmd := exec.Command(output, "version")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	return nil
}

func runDev(ctx context.Context, args []string) error {
	const binOutput = "./caddy"

	// get current/main module name
	cmd := exec.Command("go", "list", "-m")
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	currentModule := strings.TrimSpace(string(out))

	// get the root directory of the main module
	cmd = exec.Command("go", "list", "-m", "-f={{.Dir}}")
	out, err = cmd.Output()
	if err != nil {
		return err
	}
	moduleDir := strings.TrimSpace(string(out))

	// make sure the module being developed is replaced
	// so that the local copy is used
	replacements := []xcaddy.Replace{
		{
			Old: currentModule,
			New: moduleDir,
		},
	}

	// replace directives only apply to the top-level/main go.mod,
	// and since this tool is a carry-through for the user's actual
	// go.mod, we need to transfer their replace directives through
	// to the one we're making
	cmd = exec.Command("go", "list", "-m", "-f={{if .Replace}}{{.Path}} => {{.Replace}}{{end}}", "all")
	out, err = cmd.Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(line, "=>")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		replacements = append(replacements, xcaddy.Replace{
			Old: strings.TrimSpace(parts[0]),
			New: strings.TrimSpace(parts[1]),
		})
	}

	// build caddy with this module plugged in
	builder := xcaddy.Builder{
		CaddyVersion: caddyVersion,
		Plugins: []xcaddy.Dependency{
			{ModulePath: currentModule},
		},
		Replacements: replacements,
	}
	err = builder.Build(ctx, binOutput)
	if err != nil {
		return err
	}

	log.Printf("[INFO] Running %v\n\n", append([]string{binOutput}, args...))

	cmd = exec.Command(binOutput, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		return err
	}
	defer func() {
		err = os.Remove(binOutput)
		if err != nil && !os.IsNotExist(err) {
			log.Printf("[ERROR] Deleting temporary binary %s: %v", binOutput, err)
		}
	}()

	return cmd.Wait()
}

func trapSignals(ctx context.Context, cancel context.CancelFunc) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	select {
	case <-sig:
		log.Printf("[INFO] SIGINT: Shutting down")
		cancel()
	case <-ctx.Done():
		return
	}
}

func splitWith(arg string) (module, version, replace string, err error) {
	const versionSplit, replaceSplit = "@", "="

	parts := strings.SplitN(arg, versionSplit, 2)
	module = parts[0]

	if len(parts) == 1 {
		parts := strings.SplitN(module, replaceSplit, 2)
		if len(parts) > 1 {
			module = parts[0]
			replace = parts[1]
		}
	} else {
		version = parts[1]
		parts := strings.SplitN(version, replaceSplit, 2)
		if len(parts) > 1 {
			version = parts[0]
			replace = parts[1]
		}
	}

	if module == "" {
		err = fmt.Errorf("module name is required")
	}

	return
}

// Package build provides a pipe that can build binaries for several
// languages.
package build

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/apex/log"
	"github.com/goreleaser/goreleaser/internal/ids"
	"github.com/goreleaser/goreleaser/internal/logext"
	"github.com/goreleaser/goreleaser/internal/semerrgroup"
	"github.com/goreleaser/goreleaser/internal/tmpl"
	builders "github.com/goreleaser/goreleaser/pkg/build"
	"github.com/goreleaser/goreleaser/pkg/config"
	"github.com/goreleaser/goreleaser/pkg/context"
	"github.com/mattn/go-shellwords"

	// langs to init.
	_ "github.com/goreleaser/goreleaser/internal/builders/golang"
)

// Pipe for build.
type Pipe struct{}

func (Pipe) String() string {
	return "building binaries"
}

// Run the pipe.
func (Pipe) Run(ctx *context.Context) error {
	for _, build := range ctx.Config.Builds {
		if build.Skip {
			log.WithField("id", build.ID).Info("skip is set")
			continue
		}
		log.WithField("build", build).Debug("building")
		if err := runPipeOnBuild(ctx, build); err != nil {
			return err
		}
	}
	return nil
}

// Default sets the pipe defaults.
func (Pipe) Default(ctx *context.Context) error {
	ids := ids.New("builds")
	for i, build := range ctx.Config.Builds {
		build, err := buildWithDefaults(ctx, build)
		if err != nil {
			return err
		}
		ctx.Config.Builds[i] = build
		ids.Inc(ctx.Config.Builds[i].ID)
	}
	if len(ctx.Config.Builds) == 0 {
		build, err := buildWithDefaults(ctx, ctx.Config.SingleBuild)
		if err != nil {
			return err
		}
		ctx.Config.Builds = []config.Build{build}
	}
	return ids.Validate()
}

func buildWithDefaults(ctx *context.Context, build config.Build) (config.Build, error) {
	if build.Lang == "" {
		build.Lang = "go"
	}
	if build.Binary == "" {
		build.Binary = ctx.Config.ProjectName
	}
	if build.ID == "" {
		build.ID = ctx.Config.ProjectName
	}
	for k, v := range build.Env {
		build.Env[k] = os.ExpandEnv(v)
	}
	return builders.For(build.Lang).WithDefaults(build)
}

func runPipeOnBuild(ctx *context.Context, build config.Build) error {
	build, err := proxy(ctx, build)
	if err != nil {
		return err
	}

	g := semerrgroup.New(ctx.Parallelism)
	for _, target := range build.Targets {
		target := target
		build := build
		g.Go(func() error {
			opts, err := buildOptionsForTarget(ctx, build, target)
			if err != nil {
				return err
			}

			if err := runHook(ctx, *opts, build.Env, build.Hooks.Pre); err != nil {
				return fmt.Errorf("pre hook failed: %w", err)
			}
			if err := doBuild(ctx, build, *opts); err != nil {
				return err
			}
			if !ctx.SkipPostBuildHooks {
				if err := runHook(ctx, *opts, build.Env, build.Hooks.Post); err != nil {
					return fmt.Errorf("post hook failed: %w", err)
				}
			}
			return nil
		})
	}

	return g.Wait()
}

func proxy(ctx *context.Context, build config.Build) (config.Build, error) {
	if !build.IsProxied() {
		return build, nil
	}

	template := tmpl.New(ctx)

	proxy, err := template.Apply(build.Proxy.Path)
	if err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	version, err := template.Apply(build.Proxy.Version)
	if err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	log := log.WithField("id", build.ID)
	log.Infof("proxying %s@%s", proxy, version)

	template = template.WithExtraFields(tmpl.Fields{
		"Proxy":   proxy,
		"Version": version,
	})

	mod, err := template.Apply(`
module {{ .ProjectName }}

require {{ .Proxy }} {{ .Version }}

`)
	if err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	main, err := template.Apply(`
// +build main
package main

import _ "{{ .Proxy }}"
`)
	if err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	dir := fmt.Sprintf("%s/build_%s", ctx.Config.Dist, build.ID)

	log.Debugf("creating needed files")

	if err := os.Mkdir(dir, 0o755); err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	if err := os.WriteFile(dir+"/main.go", []byte(main), 0o650); err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	if err := os.WriteFile(dir+"/go.mod", []byte(mod), 0o650); err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	sumr, err := os.OpenFile("go.sum", os.O_RDONLY, 0o650)
	if err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	sumw, err := os.OpenFile(dir+"/go.sum", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o650)
	if err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	if _, err := io.Copy(sumw, sumr); err != nil {
		return build, fmt.Errorf("failed to proxy module: %w", err)
	}

	log.Debugf("tidying")
	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return build, fmt.Errorf("failed to proxy module: %w: %s", err, string(out))
	}

	build.Main = proxy
	build.Dir = dir
	return build, nil
}

func runHook(ctx *context.Context, opts builders.Options, buildEnv []string, hooks config.BuildHooks) error {
	if len(hooks) == 0 {
		return nil
	}

	for _, hook := range hooks {
		var env []string

		env = append(env, ctx.Env.Strings()...)
		env = append(env, buildEnv...)

		for _, rawEnv := range hook.Env {
			e, err := tmpl.New(ctx).WithBuildOptions(opts).Apply(rawEnv)
			if err != nil {
				return err
			}
			env = append(env, e)
		}

		dir, err := tmpl.New(ctx).WithBuildOptions(opts).Apply(hook.Dir)
		if err != nil {
			return err
		}

		sh, err := tmpl.New(ctx).WithBuildOptions(opts).
			WithEnvS(env).
			Apply(hook.Cmd)
		if err != nil {
			return err
		}

		log.WithField("hook", sh).Info("running hook")
		cmd, err := shellwords.Parse(sh)
		if err != nil {
			return err
		}

		if err := run(ctx, dir, cmd, env); err != nil {
			return err
		}
	}

	return nil
}

func doBuild(ctx *context.Context, build config.Build, opts builders.Options) error {
	return builders.For(build.Lang).Build(ctx, build, opts)
}

func buildOptionsForTarget(ctx *context.Context, build config.Build, target string) (*builders.Options, error) {
	ext := extFor(target, build.Flags)
	var goos string
	var goarch string

	if strings.Contains(target, "_") {
		goos = strings.Split(target, "_")[0]
		goarch = strings.Split(target, "_")[1]
	}

	buildOpts := builders.Options{
		Target: target,
		Ext:    ext,
		Os:     goos,
		Arch:   goarch,
	}

	binary, err := tmpl.New(ctx).WithBuildOptions(buildOpts).Apply(build.Binary)
	if err != nil {
		return nil, err
	}

	build.Binary = binary
	name := build.Binary + ext
	path, err := filepath.Abs(
		filepath.Join(
			ctx.Config.Dist,
			fmt.Sprintf("%s_%s", build.ID, target),
			name,
		),
	)
	if err != nil {
		return nil, err
	}

	log.WithField("binary", path).Info("building")
	buildOpts.Name = name
	buildOpts.Path = path
	return &buildOpts, nil
}

func extFor(target string, flags config.FlagArray) string {
	if strings.Contains(target, "windows") {
		for _, s := range flags {
			if s == "-buildmode=c-shared" {
				return ".dll"
			}
			if s == "-buildmode=c-archive" {
				return ".lib"
			}
		}
		return ".exe"
	}
	if target == "js_wasm" {
		return ".wasm"
	}
	return ""
}

func run(ctx *context.Context, dir string, command, env []string) error {
	/* #nosec */
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	entry := log.WithField("cmd", command)
	cmd.Env = env
	var b bytes.Buffer
	cmd.Stderr = io.MultiWriter(logext.NewErrWriter(entry), &b)
	cmd.Stdout = io.MultiWriter(logext.NewWriter(entry), &b)
	if dir != "" {
		cmd.Dir = dir
	}
	entry.WithField("env", env).Debug("running")
	if err := cmd.Run(); err != nil {
		entry.WithError(err).Debug("failed")
		return fmt.Errorf("%q: %w", b.String(), err)
	}
	return nil
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/source/containerimage"
	"github.com/moby/buildkit/util/system"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const sourceCommit = "22f540863e3e3bf854bbfb37530de5c0d5305e88"
const baseImage = "docker.io/library/golang:1.27@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e"
const buildCommand = "go build -mod=readonly -trimpath -o /stoke ./cmd/stoke && echo STOKE_BUILD_EXECUTED && sha256sum /stoke"

func (e *engine) definition(ctx context.Context, sessionID string) (*llb.Definition, error) {
	started := time.Now()
	platform, err := platforms.Parse(e.platform)
	if err != nil {
		return nil, err
	}
	id, err := containerimage.NewImageIdentifier(baseImage)
	if err != nil {
		return nil, err
	}
	id.Platform = &platform
	metadata, err := e.worker.ImageSource.ResolveImageMetadata(ctx, id, &sourceresolver.ResolveImageOpt{Platform: &platform}, e.sessions, session.NewGroup(sessionID))
	if err != nil {
		return nil, fmt.Errorf("resolve pinned Go image: %w", err)
	}
	var parent v1.Image
	if err := json.Unmarshal(metadata.Config, &parent); err != nil {
		return nil, err
	}
	if !platforms.OnlyStrict(platform).Match(parent.Platform) {
		return nil, fmt.Errorf("pinned base platform does not match %s", e.platform)
	}
	builder, err := llb.Image(baseImage, llb.Platform(platform)).WithImageConfig(metadata.Config)
	if err != nil {
		return nil, err
	}
	builder = builder.File(llb.Mkdir("/workspace", 0755, llb.WithParents(true))).Dir("/workspace")
	for _, env := range [][2]string{{"CGO_ENABLED", "0"}, {"GOTOOLCHAIN", "local"}, {"GOMAXPROCS", "1"}, {"GOMEMLIMIT", "768MiB"}, {"GOFLAGS", "-p=1"}} {
		builder = builder.AddEnv(env[0], env[1])
	}
	source := llb.Local("context", llb.IncludePatterns([]string{"go.mod", "go.sum", "cmd", "cmd/**", "internal", "internal/**"}))
	copyOptions := &llb.CopyInfo{CreateDestPath: true, CopyDirContentsOnly: true}
	builder = builder.File(llb.Copy(source, "go.mod", "/workspace/go.mod", copyOptions).Copy(source, "go.sum", "/workspace/go.sum", copyOptions))
	builder = builder.File(llb.Copy(source, "cmd", "/workspace/cmd", copyOptions))
	builder = builder.File(llb.Copy(source, "internal", "/workspace/internal", copyOptions))
	var shellConfig struct{ Config struct{ Shell []string } }
	if err := json.Unmarshal(metadata.Config, &shellConfig); err != nil {
		return nil, err
	}
	shell := shellConfig.Config.Shell
	if len(shell) == 0 {
		shell = []string{"/bin/sh", "-c"}
	}
	// Default LLB networking uses the worker's private RootlessKit network.
	// Explicit NetMode_HOST would request an unnecessary network.host entitlement.
	builder = builder.Run(llb.Args(append(slices.Clone(shell), buildCommand))).Root()
	final := llb.Scratch().File(llb.Copy(builder, "/stoke", "/stoke"))
	e.imageConfig, err = json.Marshal(scratchImageConfig(platform))
	if err != nil {
		return nil, err
	}
	definition, err := final.Marshal(ctx, llb.Platform(platform))
	emit(map[string]any{"phase": "definition-base-metadata", "seconds": time.Since(started).Seconds(), "base": baseImage, "selectedBaseDigest": metadata.Digest, "baseConfigDigest": digest.FromBytes(metadata.Config), "sourceCommit": sourceCommit, "platform": platforms.Format(platform)})
	return definition, err
}

func scratchImageConfig(platform v1.Platform) v1.Image {
	epoch := time.Unix(0, 0).UTC()
	return v1.Image{
		Platform: platform, Created: &epoch,
		Config: v1.ImageConfig{Env: []string{"PATH=" + system.DefaultPathEnvUnix}, WorkingDir: "/"},
		// The frontend owns authored history; imported ref descriptions differ.
		History: []v1.History{{Created: &epoch, CreatedBy: "COPY /stoke /stoke"}},
	}
}

package engine

import (
	"context"

	cooprwire "github.com/strio-dev/coopr/api"
	cooprclient "github.com/strio-dev/coopr/client"
)

// ErrCleanupFailed marks failed or unconfirmed engine cleanup, even when the
// original execution was canceled. Callers must not classify it as plain cancellation.
var ErrCleanupFailed = cooprclient.ErrCleanupFailed

var ErrUnsupportedVersion = cooprclient.ErrUnsupportedVersion

type HTTPError = cooprclient.HTTPError

type Client struct{ sdk *cooprclient.Client }

func NewClient(socket string) (*Client, error) {
	client, err := cooprclient.New(socket, cooprclient.Options{})
	if err != nil {
		return nil, err
	}
	return &Client{sdk: client}, nil
}

func (c *Client) Info(ctx context.Context) (cooprwire.Info, error) {
	return c.sdk.Info(ctx)
}

// Negotiate preflights workflow requirements using the SDK's cached service info.
func (c *Client) Negotiate(ctx context.Context, required ...string) (cooprwire.Info, error) {
	return c.sdk.Negotiate(ctx, required...)
}

func (c *Client) CloseIdleConnections() { c.sdk.CloseIdleConnections() }

func (c *Client) Execute(ctx context.Context, p Program, o Options) (Result, error) {
	return c.invoke(ctx, cooprwire.Request{Kind: "execute", Program: &p, Options: options(o)}, o)
}

func (c *Client) Build(ctx context.Context, b cooprwire.Build, component bool, o Options) (Result, error) {
	kind := "build"
	if component {
		kind = "component"
	}
	return c.invoke(ctx, cooprwire.Request{Kind: kind, Build: &b, Options: options(o)}, o)
}

// LoadComponent imports a verified component graph into the caller-selected store.
func (c *Client) LoadComponent(ctx context.Context, load cooprwire.ComponentLoad, o Options) (Result, error) {
	return c.invoke(ctx, cooprwire.Request{Kind: "component-load", ComponentLoad: &load}, o)
}

func (c *Client) Publish(ctx context.Context, p cooprwire.Publish, o Options) (Result, error) {
	return c.invoke(ctx, cooprwire.Request{Kind: "publish", Publish: &p, Options: options(o)}, o)
}

func (c *Client) invoke(ctx context.Context, payload cooprwire.Request, o Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := ValidateRegistryCaches(o.Cache); err != nil {
		return Result{}, err
	}
	value, err := c.sdk.Execute(ctx, payload, cooprclient.StreamOptions{Stdout: o.Stdout, Stderr: o.Stderr, Progress: o.Progress})
	return result(value), err
}

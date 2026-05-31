// Package devcontainers handles Docker devcontainer discovery.
const docker = @import("docker.zig");

pub const DockerClient = docker.DockerClient;
pub const Watcher = docker.Watcher;

// Package remote handles remote access setup.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// setupRemote sets up remote access via Tailscale.
pub fn setupRemote(allocator: Allocator) !void {
    _ = allocator;
    // In real implementation, this would configure Tailscale
}

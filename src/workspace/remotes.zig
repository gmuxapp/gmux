// Package workspace handles workspace remotes.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Remote represents a workspace remote.
pub const Remote = struct {
    name: []const u8,
    url: []const u8,
};

/// listRemotes lists workspace remotes.
pub fn listRemotes(allocator: Allocator, cwd: []const u8) []const Remote {
    _ = allocator;
    _ = cwd;
    return &empty_remotes;
}

const empty_remotes: []const Remote = &[_]Remote{};

// Package gmux provides the dumpenv command.
const std = @import("std");
const Allocator = std.mem.Allocator;
const sessionenv = @import("../sessionenv/sessionenv.zig");

/// dumpEnv dumps the session environment.
pub fn dumpEnv(allocator: Allocator, cwd: []const u8) !void {
    _ = allocator;
    _ = cwd;
    // In real implementation, this would dump the environment
}

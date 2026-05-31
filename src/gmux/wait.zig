// Package gmux provides the wait command.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// wait waits for a session to exit.
pub fn wait(allocator: Allocator, sessionId: []const u8) !void {
    _ = allocator;
    _ = sessionId;
    // In real implementation, this would wait for the session
}

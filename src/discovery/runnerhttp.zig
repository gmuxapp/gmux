// Package runnerhttp handles HTTP communication with runners.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// queryMeta queries a runner's /meta endpoint.
pub fn queryMeta(allocator: Allocator, socketPath: []const u8) ![]const u8 {
    _ = allocator;
    _ = socketPath;
    // In real implementation, this would connect to the Unix socket
    return "";
}

/// sendInput sends input to a runner.
pub fn sendInput(allocator: Allocator, socketPath: []const u8, data: []const u8) !void {
    _ = allocator;
    _ = socketPath;
    _ = data;
    // In real implementation, this would connect to the Unix socket
}

/// killSession kills a runner session.
pub fn killSession(socketPath: []const u8) !void {
    _ = socketPath;
    // In real implementation, this would connect to the Unix socket
}

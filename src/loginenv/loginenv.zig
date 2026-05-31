// Package loginenv captures login environment.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// captureLoginEnv captures the login environment.
pub fn captureLoginEnv(allocator: Allocator, cwd: []const u8) ![]const []const u8 {
    _ = allocator;
    _ = cwd;
    // In real implementation, this would exec a login shell and capture env
    return &.{
        "PATH=/usr/local/bin:/usr/bin:/bin",
        "HOME=/home/user",
        "SHELL=/bin/bash",
    };
}

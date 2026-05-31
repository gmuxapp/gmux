// Package gmux provides the daemon auto-start.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// ensureGmuxd ensures gmuxd is running.
pub fn ensureGmuxd(allocator: Allocator) !void {
    _ = allocator;
    // In real implementation, this would start gmuxd if not running
}

/// gmuxdClient returns the gmuxd HTTP client.
pub fn gmuxdClient(allocator: Allocator) !*anyopaque {
    _ = allocator;
    return null;
}

/// gmuxdBaseURL returns the gmuxd base URL.
pub fn gmuxdBaseURL() []const u8 {
    return "http://localhost:8125";
}

// Package gmux provides the daemon handshake.
const std = @import("std");
const Allocator = std.mem.Allocator;
const unixipc = @import("../unixipc/unixipc.zig");

/// handshake performs the handshake with gmuxd.
pub fn handshake(allocator: Allocator, socketPath: []const u8) !struct { version: []const u8, auth: []const u8 } {
    _ = allocator;
    _ = socketPath;
    return .{ .version = "", .auth = "" };
}

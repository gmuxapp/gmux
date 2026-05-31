// Package naming generates unique session identifiers.
const std = @import("std");

/// SessionID generates a unique session identifier like "sess-1a2b3c4d".
pub fn sessionID(allocator: Allocator) ![]const u8 {
    var b: [4]u8 = undefined;
    try std.crypto.random.bytes(&b);
    return try std.fmt.allocPrint(allocator, "sess-{x:x:x:x}", .{ b[0], b[1], b[2], b[3] });
}

const Allocator = std.mem.Allocator;

test "sessionID format" {
    const allocator = testing.allocator;
    const id = try sessionID(allocator);
    defer allocator.free(id);
    try std.testing.expect(std.mem.startsWith(u8, id, "sess-"));
    try std.testing.expectEqual(@as(usize, 13), id.len);
}

const testing = std.testing;

// Package sessionenv filters gmux session-identity environment
// variables out of a process environment before it is handed to a
// child that must not inherit the parent session's identity.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Strip returns env with gmux session-identity variables removed.
/// GMUX_SOCKET_DIR is preserved as it's configuration, not identity.
pub fn strip(allocator: Allocator, env: []const []const u8) ![][]u8 {
    var out = std.ArrayList([]u8).empty;
    errdefer {
        for (out.items) |item| {
            allocator.free(item);
        }
        out.deinit(allocator);
    }

    for (env) |e| {
        const key = getKey(e);

        // Skip bare GMUX marker
        if (std.mem.eql(u8, key, "GMUX")) {
            continue;
        }

        // Skip GMUX_* except preserved configuration vars
        if (std.mem.startsWith(u8, key, "GMUX_")) {
            if (!std.mem.eql(u8, key, "GMUX_SOCKET_DIR")) {
                continue;
            }
        }

        try out.append(allocator, try allocator.dupe(u8, e));
    }

    return out.toOwnedSlice(allocator);
}

fn getKey(e: []const u8) []const u8 {
    if (std.mem.indexOfScalar(u8, e, '=')) |idx| {
        return e[0..idx];
    }
    return e;
}

test "strip removes GMUX" {
    const allocator = testing.allocator;
    const env = [_][]const u8{ "GMUX=1", "HOME=/home/user", "PATH=/usr/bin" };

    const result = try strip(allocator, &env);
    defer {
        for (result) |r| allocator.free(r);
        allocator.free(result);
    }

    try std.testing.expectEqual(@as(usize, 2), result.len);
    try std.testing.expect(std.mem.eql(u8, result[0], "HOME=/home/user"));
    try std.testing.expect(std.mem.eql(u8, result[1], "PATH=/usr/bin"));
}

test "strip preserves GMUX_SOCKET_DIR" {
    const allocator = testing.allocator;
    const env = [_][]const u8{ "GMUX=1", "GMUX_SOCKET_DIR=/tmp/gmux", "GMUX_SESSION_ID=abc" };

    const result = try strip(allocator, &env);
    defer {
        for (result) |r| allocator.free(r);
        allocator.free(result);
    }

    try std.testing.expectEqual(@as(usize, 1), result.len);
    try std.testing.expect(std.mem.eql(u8, result[0], "GMUX_SOCKET_DIR=/tmp/gmux"));
}

test "strip preserves GMUXD_ vars" {
    const allocator = testing.allocator;
    const env = [_][]const u8{ "GMUXD_LISTEN_ADDR=0.0.0.0:8790", "GMUX=1" };

    const result = try strip(allocator, &env);
    defer {
        for (result) |r| allocator.free(r);
        allocator.free(result);
    }

    try std.testing.expectEqual(@as(usize, 1), result.len);
    try std.testing.expect(std.mem.eql(u8, result[0], "GMUXD_LISTEN_ADDR=0.0.0.0:8790"));
}

test "strip empty" {
    const allocator = testing.allocator;
    const env = [_][]const u8{};

    const result = try strip(allocator, &env);
    defer {
        for (result) |r| allocator.free(r);
        allocator.free(result);
    }

    try std.testing.expectEqual(@as(usize, 0), result.len);
}

const testing = std.testing;

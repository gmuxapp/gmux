// Package authtoken manages the gmuxd authentication token.
const std = @import("std");
const Allocator = std.mem.Allocator;
const paths = @import("../paths/paths.zig");

const TokenFile = "auth-token";

/// generate creates a new random auth token.
pub fn generate(allocator: Allocator) ![]const u8 {
    var bytes: [32]u8 = undefined;
    try std.crypto.random.bytes(&bytes);
    return std.fmt.allocPrint(allocator, "{x}", .{bytes});
}

/// save writes the auth token to disk.
pub fn save(allocator: Allocator, token: []const u8) !void {
    const stateDir = try paths.stateDir(allocator);
    defer allocator.free(stateDir);

    const tokenPath = try std.fs.path.join(allocator, &.{ stateDir, TokenFile });
    defer allocator.free(tokenPath);

    const file = try std.fs.createFileAbsolute(tokenPath, .{ .mode = 0o600 });
    defer file.close();

    try file.writeAll(token);
}

/// load reads the auth token from disk.
pub fn load(allocator: Allocator) !?[]const u8 {
    const stateDir = try paths.stateDir(allocator);
    defer allocator.free(stateDir);

    const tokenPath = try std.fs.path.join(allocator, &.{ stateDir, TokenFile });
    defer allocator.free(tokenPath);

    const file = std.fs.openFileAbsolute(tokenPath, .{}) catch |err| {
        if (err == error.FileNotFound) return null;
        return err;
    };
    defer file.close();

    const content = try file.readToEndAlloc(allocator, 256);
    const token = std.mem.trim(u8, content, " \t\r\n");

    return if (token.len > 0) allocator.dupe(u8, token) else null;
}

/// remove deletes the auth token file.
pub fn remove(allocator: Allocator) !void {
    const stateDir = try paths.stateDir(allocator);
    defer allocator.free(stateDir);

    const tokenPath = try std.fs.path.join(allocator, &.{ stateDir, TokenFile });
    defer allocator.free(tokenPath);

    std.fs.deleteFileAbsolute(tokenPath) catch {};
}

test "generate token" {
    const allocator = testing.allocator;
    const token = try generate(allocator);
    defer allocator.free(token);
    try std.testing.expectEqual(@as(usize, 64), token.len);
}

const testing = std.testing;

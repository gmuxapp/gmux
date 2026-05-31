// Package binhash computes the sha256 hash of the running executable.
const std = @import("std");

var once = std.ThreadOnce{};
var hash: []const u8 = "";

/// Self returns the hex-encoded sha256 hash of the current executable.
/// Computed once, cached for the process lifetime. Returns "" on error.
pub fn self() []const u8 {
    once.call(computeHash);
    return hash;
}

fn computeHash() void {
    const exe = std.posix.realpathAlloc(std.heap.c_allocator, "/proc/self/exe") catch {
        // Fallback: try to get executable path
        return;
    };
    defer std.heap.c_allocator.free(exe);

    const file = std.fs.openFileAbsolute(exe, .{}) catch return;
    defer file.close();

    const content = file.readToEndAlloc(std.heap.c_allocator, max_file_size) catch return;
    defer std.heap.c_allocator.free(content);

    const h = std.crypto.hash.sha2.Sh256.hash(content);
    hash = std.fmt.bufPrint(hash_buf, "{x}", .{h}) catch "";
}

const max_file_size = 100 * 1024 * 1024; // 100 MB
var hash_buf: [64]u8 = undefined;

test "binhash" {
    const h = self();
    // Should be either 64 hex chars or empty
    if (h.len > 0) {
        try std.testing.expectEqual(@as(usize, 64), h.len);
    }
}

const testing = std.testing;

// Package unixipc provides Unix socket IPC utilities.
const std = @import("std");
const Allocator = std.mem.Allocator;
const paths = @import("../paths/paths.zig");

/// connect connects to a Unix socket.
pub fn connect(socketPath: []const u8) !std.posix.Socket {
    var addr = try std.posix.sockaddr.initUnix(socketPath);
    const sock = try std.posix.socket(.{ .family = .unix, .type = .stream, .protocol = .def });

    try std.posix.connect(sock, &addr);

    return sock;
}

/// writeAll writes data to a Unix socket.
pub fn writeAll(sock: std.posix.Socket, data: []const u8) !void {
    var remaining = data;
    while (remaining.len > 0) {
        const n = try std.posix.write(sock, remaining);
        remaining = remaining[n..];
    }
}

/// readAll reads all data from a Unix socket until EOF.
pub fn readAll(sock: std.posix.Socket, allocator: Allocator) ![]const u8 {
    var stream = std.io.fixedBufferStream(try allocator.alloc(u8, 65536));
    errdefer allocator.free(stream.getWritten());

    while (true) {
        const n = std.posix.read(sock, stream.getRemaining()) catch |err| {
            if (err == error.ConnectionReset or err == error.BrokenPipe) break;
            return err;
        };
        if (n == 0) break;
        stream.advance(n);
    }

    return stream.getWritten();
}

/// close closes a Unix socket.
pub fn close(sock: std.posix.Socket) void {
    std.posix.close(sock);
}

/// shutdown sends a shutdown request to gmuxd via the Unix socket.
pub fn shutdown(socketPath: []const u8) !bool {
    const sock = connect(socketPath) catch return false;
    defer close(sock);

    try writeAll(sock, "SHUTDOWN\r\n");

    return true;
}

/// health checks if gmuxd is healthy via the Unix socket.
pub fn healthy(socketPath: []const u8) bool {
    const sock = connect(socketPath) catch return false;
    defer close(sock);

    return true;
}

/// healthVersion returns the gmuxd version via the Unix socket.
pub fn healthVersion(socketPath: []const u8) struct { version: []const u8, ok: bool } {
    _ = socketPath;
    return .{ .version = "", .ok = false };
}

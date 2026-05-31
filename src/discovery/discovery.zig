// Package discovery scans for Unix sockets and queries their /meta endpoint.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");
const sessionmeta = @import("../sessionmeta/sessionmeta.zig");

/// ExpectedRunnerHash is the sha256 hash of the gmux binary.
var expectedRunnerHash: []const u8 = "";

/// socketDir returns the directory where session sockets live.
pub fn socketDir(allocator: Allocator) ![]const u8 {
    if (std.process.getEnvVarOwned(allocator, "GMUX_SOCKET_DIR")) |dir| {
        return dir;
    }
    return allocator.dupe(u8, "/tmp/gmux-sessions");
}

/// OnDeadFunc is invoked after a session has just landed as Alive=false.
pub const OnDeadFunc = *const fn (sess: *store.Session) callconv(.C) void;

/// Scan finds all .sock files and queries each runner's /meta endpoint.
pub fn scan(sessions: *store.Store, allocator: Allocator, onDead: ?OnDeadFunc) !void {
    const dir = try socketDir(allocator);
    defer allocator.free(dir);

    var dir_handle = std.fs.openDirAbsolute(dir, .{}) catch |err| {
        if (err == error.FileNotFound) return;
        return err;
    };
    defer dir_handle.close();

    var it = dir_handle.iterate();
    while (try it.next()) |entry| {
        if (entry.kind != .file) continue;
        if (!std.mem.endsWith(u8, entry.name, ".sock")) continue;

        const socketPath = try std.fs.path.join(allocator, &.{ dir, entry.name });
        defer allocator.free(socketPath);

        // Query /meta endpoint
        const meta = try queryMeta(allocator, socketPath);
        if (meta) |m| {
            // Upsert session
            var sess = try store.Session.init(allocator, m.id, m.kind, m.command);
            sess.cwd = try allocator.dupe(u8, m.cwd);
            sess.alive = m.alive;
            sessions.upsert(sess);
        } else {
            // Socket unreachable - session died
            if (onDead) |callback| {
                _ = callback;
            }
        }
    }
}

/// MetaResponse is the response from GET /meta.
const MetaResponse = struct {
    id: []const u8,
    kind: []const u8,
    command: []const []const u8,
    cwd: []const u8,
    alive: bool,
    pid: i32 = 0,
    socketPath: []const u8 = "",
    slug: []const u8 = "",
    title: []const u8 = "",
    status: ?struct { label: []const u8, working: bool, error_: bool } = null,
};

/// queryMeta queries the runner's /meta endpoint via Unix socket.
fn queryMeta(allocator: Allocator, socketPath: []const u8) !?MetaResponse {
    // Connect to Unix socket
    var addr = try std.posix.sockaddr.initUnix(socketPath);
    const sock = try std.posix.socket(.{ .family = .unix, .type = .stream, .protocol = .def });
    errdefer std.posix.close(sock);

    try std.posix.connect(sock, &addr);

    // Send HTTP request
    const request = "GET /meta HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n";
    try std.posix.writeAll(sock, request);

    // Read response
    var buffer: [4096]u8 = undefined;
    var total: usize = 0;
    while (total < buffer.len) {
        const n = std.posix.read(sock, buffer[total..]) catch break;
        if (n == 0) break;
        total += n;
    }
    std.posix.close(sock);

    const response = buffer[0..total];

    // Parse HTTP response
    const body_start = std.mem.indexOf(u8, response, "\r\n\r\n") orelse return null;
    const body = response[body_start + 4..];

    // Parse JSON
    return std.json.parseFromSlice(MetaResponse, allocator, body, .{});
}

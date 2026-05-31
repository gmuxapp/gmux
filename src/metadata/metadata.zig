// Package metadata persists session metadata to disk.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// MetaDir is the directory where session metadata is stored.
pub const MetaDir = "/tmp/gmux-meta";

/// SessionMeta is the per-session metadata record.
pub const SessionMeta = struct {
    version: u32 = 1,
    sessionId: []const u8,
    kind: []const u8,
    command: []const []const u8,
    cwd: []const u8,
    state: []const u8 = "starting",
    createdAt: f64,
    updatedAt: f64,
    pid: ?i32 = null,
    exitCode: ?i32 = null,
    error_: []const u8 = "",
    socketPath: []const u8 = "",
    sessionFile: []const u8 = "",
    sessionFileExists: bool = false,

    pub fn init(allocator: Allocator, sessionId: []const u8, kind: []const u8, cwd: []const u8, command: []const []const u8) !SessionMeta {
        const now = timestampNow();
        return SessionMeta{
            .sessionId = try allocator.dupe(u8, sessionId),
            .kind = try allocator.dupe(u8, kind),
            .command = try dupSlice(allocator, command),
            .cwd = try allocator.dupe(u8, cwd),
            .createdAt = now,
            .updatedAt = now,
        };
    }

    pub fn deinit(self: *SessionMeta, allocator: Allocator) void {
        allocator.free(self.sessionId);
        allocator.free(self.kind);
        for (self.command) |c| allocator.free(c);
        allocator.free(self.command);
        allocator.free(self.cwd);
        allocator.free(self.error_);
        allocator.free(self.socketPath);
        allocator.free(self.sessionFile);
    }

    fn metaPath(allocator: Allocator, sessionId: []const u8) ![]const u8 {
        return std.fs.path.join(allocator, &.{ MetaDir, sessionId ++ ".json" });
    }

    pub fn write(self: *SessionMeta, allocator: Allocator) !void {
        try std.fs.cwd().makePath(MetaDir);
        self.updatedAt = timestampNow();

        const path = try metaPath(allocator, self.sessionId);
        defer allocator.free(path);

        const json = try toJson(allocator, self);
        defer allocator.free(json);

        const file = try std.fs.createFileAbsolute(path, .{ .mode = 0o644 });
        defer file.close();
        try file.writeAll(json);
    }

    pub fn setState(self: *SessionMeta, allocator: Allocator, state: []const u8) !void {
        allocator.free(self.state);
        self.state = try allocator.dupe(u8, state);
        try self.write(allocator);
    }

    pub fn setRunning(self: *SessionMeta, allocator: Allocator, pid: i32) !void {
        try self.setState(allocator, "running");
        self.pid = pid;
        try self.write(allocator);
    }

    pub fn setExited(self: *SessionMeta, allocator: Allocator, exitCode: i32) !void {
        try self.setState(allocator, "exited");
        self.exitCode = exitCode;
        try self.write(allocator);
    }

    pub fn setError(self: *SessionMeta, allocator: Allocator, msg: []const u8) !void {
        try self.setState(allocator, "error");
        allocator.free(self.error_);
        self.error_ = try allocator.dupe(u8, msg);
        try self.write(allocator);
    }

    pub fn cleanup(self: *SessionMeta, allocator: Allocator) !void {
        const path = try metaPath(allocator, self.sessionId);
        defer allocator.free(path);
        std.fs.deleteFileAbsolute(path) catch {};
    }

    pub fn read(allocator: Allocator, sessionId: []const u8) !?SessionMeta {
        const path = try metaPath(allocator, sessionId);
        defer allocator.free(path);

        const file = std.fs.openFileAbsolute(path, .{}) catch |err| {
            if (err == error.FileNotFound) return null;
            return err;
        };
        defer file.close();

        const content = try file.readToEndAlloc(allocator, max_json_size);
        defer allocator.free(content);

        return fromJson(allocator, content);
    }

    pub fn listAll(allocator: Allocator) ![]SessionMeta {
        var result = std.ArrayList(SessionMeta).init(allocator);
        errdefer {
            for (result.items) |*m| m.deinit(allocator);
            result.deinit();
        }

        var dir = std.fs.openDirAbsolute(MetaDir, .{}) catch |err| {
            if (err == error.FileNotFound) return try result.toOwnedSlice();
            return err;
        };
        defer dir.close();

        var it = dir.iterate();
        while (try it.next()) |entry| {
            if (entry.kind != .file) continue;
            if (!std.mem.endsWith(u8, entry.name, ".json")) continue;

            const sessionId = entry.name[0 .. entry.name.len - 5];
            const meta = try read(allocator, sessionId);
            if (meta) |m| try result.append(m);
        }

        return try result.toOwnedSlice();
    }
};

fn timestampNow() f64 {
    return @as(f64, @floatFromInt(std.time.milliTimestamp())) / 1000.0;
}

fn dupSlice(allocator: Allocator, slice: []const []const u8) ![]const []const u8 {
    var result = try allocator.alloc([]const u8, slice.len);
    for (slice, 0..) |s, i| {
        result[i] = try allocator.dupe(u8, s);
    }
    return result;
}

const max_json_size = 64 * 1024;

fn toJson(allocator: Allocator, meta: *SessionMeta) ![]const u8 {
    var ws = std.json.WriteStream.init(allocator);
    defer ws.deinit();

    try ws.beginObject();
    try ws.field("version");
    try ws.write(meta.version);
    try ws.field("session_id");
    try ws.write(meta.sessionId);
    try ws.field("kind");
    try ws.write(meta.kind);
    try ws.field("command");
    try ws.write(meta.command);
    try ws.field("cwd");
    try ws.write(meta.cwd);
    try ws.field("state");
    try ws.write(meta.state);
    try ws.field("created_at");
    try ws.write(meta.createdAt);
    try ws.field("updated_at");
    try ws.write(meta.updatedAt);
    if (meta.pid) |pid| {
        try ws.field("pid");
        try ws.write(pid);
    }
    if (meta.exitCode) |ec| {
        try ws.field("exit_code");
        try ws.write(ec);
    }
    if (meta.error_.len > 0) {
        try ws.field("error");
        try ws.write(meta.error_);
    }
    if (meta.socketPath.len > 0) {
        try ws.field("socket_path");
        try ws.write(meta.socketPath);
    }
    try ws.endObject();

    return ws.getWritten();
}

fn fromJson(allocator: Allocator, json: []const u8) !SessionMeta {
    return std.json.parseFromSlice(SessionMeta, allocator, json, .{});
}

test "meta write and read" {
    const allocator = testing.allocator;
    const sessionId = "test-session-123";

    std.fs.deleteTreeAbsolute(MetaDir) catch {};
    defer std.fs.deleteTreeAbsolute(MetaDir) catch {};

    var meta = try SessionMeta.init(allocator, sessionId, "shell", "/tmp", &.{"echo", "hello"});
    defer meta.deinit(allocator);

    try meta.write(allocator);

    const read_meta = try SessionMeta.read(allocator, sessionId);
    if (read_meta) |rm| {
        defer rm.deinit(allocator);
        try std.testing.expect(std.mem.eql(u8, rm.sessionId, sessionId));
        try std.testing.expect(std.mem.eql(u8, rm.kind, "shell"));
    } else {
        try std.testing.expect(false);
    }
}

const testing = std.testing;

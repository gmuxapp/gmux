// Package sessionmeta persists per-session metadata to disk.
const std = @import("std");
const Allocator = std.mem.Allocator;
const paths = @import("../paths/paths.zig");

/// DefaultDir returns the default sessionmeta directory.
pub fn defaultDir(allocator: Allocator) ![]const u8 {
    return paths.sessionsDir(allocator);
}

/// Meta is the per-session metadata record.
pub const Meta = struct {
    id: []const u8,
    kind: []const u8,
    command: []const []const u8,
    cwd: []const u8,
    slug: []const u8 = "",
    alive: bool = false,
    pid: ?i32 = null,
    exitCode: ?i32 = null,
    socketPath: []const u8 = "",
    projectSlug: []const u8 = "",
    projectIndex: i32 = -1,
    createdAt: i64 = 0,
    updatedAt: i64 = 0,
};

/// Store manages sessionmeta persistence.
pub const Store = struct {
    allocator: Allocator,
    dir: []const u8,

    pub fn init(allocator: Allocator, dir: []const u8) !*Store {
        const self = try allocator.create(Store);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.dir = try allocator.dupe(u8, dir);

        return self;
    }

    pub fn deinit(self: *Store) void {
        self.allocator.free(self.dir);
        self.allocator.destroy(self);
    }

    pub fn getDir(self: *Store) []const u8 {
        return self.dir;
    }

    /// write persists session metadata to disk.
    pub fn write(self: *Store, meta: Meta) !void {
        const sessionDir = try std.fs.path.join(self.allocator, &.{ self.dir, meta.id });
        defer self.allocator.free(sessionDir);

        try std.fs.cwd().makePath(sessionDir);

        const metaPath = try std.fs.path.join(self.allocator, &.{ sessionDir, "meta.json" });
        defer self.allocator.free(metaPath);

        const json = try toJson(self.allocator, meta);
        defer self.allocator.free(json);

        const file = try std.fs.createFileAbsolute(metaPath, .{ .mode = 0o600 });
        defer file.close();

        try file.writeAll(json);
    }

    /// read loads session metadata from disk.
    pub fn read(self: *Store, id: []const u8) !?Meta {
        const sessionDir = try std.fs.path.join(self.allocator, &.{ self.dir, id });
        defer self.allocator.free(sessionDir);

        const metaPath = try std.fs.path.join(self.allocator, &.{ sessionDir, "meta.json" });
        defer self.allocator.free(metaPath);

        const file = std.fs.openFileAbsolute(metaPath, .{}) catch |err| {
            if (err == error.FileNotFound) return null;
            return err;
        };
        defer file.close();

        const content = try file.readToEndAlloc(self.allocator, 65536);
        defer self.allocator.free(content);

        return fromJson(self.allocator, content);
    }

    /// remove deletes session metadata from disk.
    pub fn remove(self: *Store, id: []const u8) !void {
        const sessionDir = try std.fs.path.join(self.allocator, &.{ self.dir, id });
        defer self.allocator.free(sessionDir);

        std.fs.deleteTreeAbsolute(sessionDir) catch {};
    }

    fn toJson(allocator: Allocator, meta: Meta) ![]const u8 {
        var ws = std.json.WriteStream.init(allocator);
        defer ws.deinit();

        try ws.beginObject();
        try ws.field("id");
        try ws.write(meta.id);
        try ws.field("kind");
        try ws.write(meta.kind);
        try ws.field("command");
        try ws.write(meta.command);
        try ws.field("cwd");
        try ws.write(meta.cwd);
        if (meta.slug.len > 0) {
            try ws.field("slug");
            try ws.write(meta.slug);
        }
        try ws.field("alive");
        try ws.write(meta.alive);
        try ws.endObject();

        return ws.getWritten();
    }

    fn fromJson(allocator: Allocator, json: []const u8) !Meta {
        return std.json.parseFromSlice(Meta, allocator, json, .{});
    }
};

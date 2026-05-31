// Package store provides the in-memory session store for gmuxd.
const std = @import("std");
const Allocator = std.mem.Allocator;
const adapter = @import("../adapter/adapter.zig");

/// Status is the application-reported status.
pub const Status = struct {
    label: []const u8 = "",
    working: bool = false,
    error_: bool = false,
};

/// Session is the in-memory model for a gmux session.
pub const Session = struct {
    allocator: Allocator,
    id: []const u8,
    peer: []const u8 = "",
    createdAt: []const u8 = "",
    command: []const []const u8,
    cwd: []const u8 = "",
    kind: []const u8,
    workspaceRoot: []const u8 = "",
    alive: bool = false,
    pid: i32 = 0,
    exitCode: ?i32 = null,
    startedAt: []const u8 = "",
    exitedAt: []const u8 = "",
    title: []const u8 = "",
    subtitle: []const u8 = "",
    status: ?Status = null,
    unread: bool = false,
    lastActivityAt: []const u8 = "",
    resumable: bool = false,
    socketPath: []const u8 = "",
    terminalCols: u16 = 0,
    terminalRows: u16 = 0,
    slug: []const u8 = "",
    runnerVersion: []const u8 = "",
    binaryHash: []const u8 = "",
    shellTitle: []const u8 = "",
    adapterTitle: []const u8 = "",
    projectSlug: []const u8 = "",
    projectIndex: i32 = -1,

    pub fn init(allocator: Allocator, id: []const u8, kind: []const u8, command: []const []const u8) !*Session {
        const self = try allocator.create(Session);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.id = try allocator.dupe(u8, id);
        self.kind = try allocator.dupe(u8, kind);
        self.command = try dupSlice(allocator, command);
        return self;
    }

    pub fn deinit(self: *Session) void {
        const allocator = self.allocator;
        allocator.free(self.id);
        allocator.free(self.peer);
        allocator.free(self.createdAt);
        for (self.command) |c| allocator.free(c);
        allocator.free(self.command);
        allocator.free(self.cwd);
        allocator.free(self.kind);
        allocator.free(self.workspaceRoot);
        allocator.free(self.startedAt);
        allocator.free(self.exitedAt);
        allocator.free(self.title);
        allocator.free(self.subtitle);
        allocator.free(self.lastActivityAt);
        allocator.free(self.socketPath);
        allocator.free(self.slug);
        allocator.free(self.runnerVersion);
        allocator.free(self.binaryHash);
        allocator.free(self.shellTitle);
        allocator.free(self.adapterTitle);
        allocator.free(self.projectSlug);
        allocator.destroy(self);
    }

    /// resolveTitle computes the display title: adapter > shell > command basename.
    pub fn resolveTitle(self: *Session) []const u8 {
        if (self.adapterTitle.len > 0) return self.adapterTitle;
        if (self.shellTitle.len > 0) return self.shellTitle;
        if (self.command.len > 0) {
            return std.fs.path.basename(self.command[0]);
        }
        return "";
    }

    fn dupSlice(allocator: Allocator, slice: []const []const u8) ![]const []const u8 {
        var result = try allocator.alloc([]const u8, slice.len);
        for (slice, 0..) |s, i| {
            result[i] = try allocator.dupe(u8, s);
        }
        return result;
    }
};

/// Event is a store event sent to subscribers.
pub const Event = struct {
    type_: []const u8,
    id: []const u8,
    session: ?*Session = null,
};

/// Store is the in-memory session store.
pub const Store = struct {
    allocator: Allocator,
    sessions: std.StringHashMap(*Session),
    onEvent: ?*const fn (e: Event) callconv(.C) void = null,

    pub fn init(allocator: Allocator) *Store {
        const self = allocator.create(Store) catch unreachable;
        self.allocator = allocator;
        self.sessions = std.StringHashMap(*Session).init(allocator);
        return self;
    }

    pub fn deinit(self: *Store) void {
        var it = self.sessions.iterator();
        while (it.next()) |entry| {
            entry.value_ptr.*.deinit();
        }
        self.sessions.deinit();
        self.allocator.destroy(self);
    }

    /// Get retrieves a session by id.
    pub fn get(self: *Store, id: []const u8) ?*Session {
        return self.sessions.get(id);
    }

    /// List returns all sessions.
    pub fn list(self: *Store) ![][]*Session {
        var result = std.ArrayList(*Session).init(self.allocator);
        var it = self.sessions.iterator();
        while (it.next()) |entry| {
            try result.append(entry.value_ptr.*);
        }
        return try result.toOwnedSlice();
    }

    /// Upsert adds or updates a session.
    pub fn upsert(self: *Store, session: *Session) void {
        const id = session.id;
        if (self.sessions.get(id)) |old| {
            old.deinit();
        }
        self.sessions.put(id, session) catch {};
        if (self.onEvent) |cb| cb(.{ .type_ = "session-upsert", .id = id, .session = session });
    }

    /// Remove deletes a session.
    pub fn remove(self: *Store, id: []const u8) void {
        if (self.sessions.fetchRemove(id)) |session| {
            session.deinit();
            if (self.onEvent) |cb| cb(.{ .type_ = "session-remove", .id = id });
        }
    }

    /// Update modifies a session with a callback.
    pub fn update(self: *Store, id: []const u8, callback: anytype) void {
        if (self.sessions.get(id)) |session| {
            callback(session);
            if (self.onEvent) |cb| cb(.{ .type_ = "session-upsert", .id = id, .session = session });
        }
    }
};

test "store basic operations" {
    const allocator = testing.allocator;
    var store = Store.init(allocator);
    defer store.deinit();

    var session = try Session.init(allocator, "test-123", "shell", &.{"echo", "hello"});
    defer session.deinit();

    store.upsert(session);

    const found = store.get("test-123");
    try std.testing.expect(found != null);
    try std.testing.expect(std.mem.eql(u8, found.?.id, "test-123"));

    const sessions = try store.list();
    defer allocator.free(sessions);
    try std.testing.expectEqual(@as(usize, 1), sessions.len);
}

const testing = std.testing;

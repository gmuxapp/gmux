// Package session holds the in-memory session state for a single gmux-run instance.
const std = @import("std");
const Allocator = std.mem.Allocator;
const adapter = @import("../adapter/adapter.zig");

/// Event is sent to subscribers.
pub const Event = struct {
    type_: []const u8,
    data: ?EventData = null,
};

pub const EventData = union(Enum) {
    const Enum = enum { exit, meta, status, activity, terminalResize, raw };
    exit: struct { exitCode: i32 },
    meta: struct {
        title: []const u8 = "",
        shellTitle: []const u8 = "",
        adapterTitle: []const u8 = "",
        subtitle: []const u8 = "",
        unread: ?bool = null,
        slug: []const u8 = "",
    },
    status: adapter.Status,
    activity: void,
    terminalResize: struct { cols: u16, rows: u16 },
    raw: []const u8,
};

/// Subscriber callback type.
pub const SubscriberFn = *const fn (event: Event) callconv(.C) void;

/// Config for creating a new session state.
pub const Config = struct {
    id: []const u8,
    command: []const []const u8,
    cwd: []const u8,
    kind: []const u8,
    socketPath: []const u8,
    binaryHash: []const u8 = "",
    runnerVersion: []const u8 = "",
    workspaceRoot: []const u8 = "",
};

/// State is the in-memory session state served by GET /meta.
pub const State = struct {
    allocator: Allocator,
    id: []const u8,
    createdAt: []const u8,
    command: []const []const u8,
    cwd: []const u8,
    kind: []const u8,
    workspaceRoot: []const u8,

    alive: bool = false,
    pid: i32 = 0,
    exitCode: ?i32 = null,
    startedAt: []const u8 = "",
    exitedAt: []const u8 = "",

    shellTitle: []const u8 = "",
    adapterTitle: []const u8 = "",
    subtitle: []const u8 = "",
    status: ?adapter.Status = null,
    unread: bool = false,
    slug: []const u8 = "",

    terminalCols: u16 = 0,
    terminalRows: u16 = 0,

    socketPath: []const u8,
    binaryHash: []const u8 = "",
    runnerVersion: []const u8 = "",

    subscribers: std.ArrayList(SubscriberFn),
    lastActivity: i64 = 0,

    pub fn init(allocator: Allocator, cfg: Config) !*State {
        const self = try allocator.create(State);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.id = try allocator.dupe(u8, cfg.id);
        self.createdAt = try isoNow(allocator);
        self.command = try dupSlice(allocator, cfg.command);
        self.cwd = try allocator.dupe(u8, cfg.cwd);
        self.kind = try allocator.dupe(u8, cfg.kind);
        self.workspaceRoot = try allocator.dupe(u8, cfg.workspaceRoot);
        self.socketPath = try allocator.dupe(u8, cfg.socketPath);
        self.binaryHash = try allocator.dupe(u8, cfg.binaryHash);
        self.runnerVersion = try allocator.dupe(u8, cfg.runnerVersion);
        self.subscribers = std.ArrayList(SubscriberFn).init(allocator);

        return self;
    }

    pub fn deinit(self: *State) void {
        const allocator = self.allocator;
        allocator.free(self.id);
        allocator.free(self.createdAt);
        for (self.command) |c| allocator.free(c);
        allocator.free(self.command);
        allocator.free(self.cwd);
        allocator.free(self.kind);
        allocator.free(self.workspaceRoot);
        allocator.free(self.startedAt);
        allocator.free(self.exitedAt);
        allocator.free(self.shellTitle);
        allocator.free(self.adapterTitle);
        allocator.free(self.subtitle);
        allocator.free(self.slug);
        allocator.free(self.socketPath);
        allocator.free(self.binaryHash);
        allocator.free(self.runnerVersion);
        self.subscribers.deinit();
        allocator.destroy(self);
    }

    /// Title returns the resolved display title: adapter > shell > command basename.
    pub fn title(self: *State) []const u8 {
        if (self.adapterTitle.len > 0) return self.adapterTitle;
        if (self.shellTitle.len > 0) return self.shellTitle;
        return commandBasename(self.command);
    }

    fn commandBasename(cmd: []const []const u8) []const u8 {
        if (cmd.len == 0) return "";
        if (cmd.len == 1) return std.fs.path.basename(cmd[0]);
        var parts = std.ArrayList([]const u8).init(std.heap.c_allocator);
        parts.append(std.fs.path.basename(cmd[0])) catch return "";
        for (cmd[1..]) |c| parts.append(c) catch return "";
        return std.mem.join(std.heap.c_allocator, " ", parts.items) catch "";
    }

    /// SetRunning marks the session as alive with the given PID.
    pub fn setRunning(self: *State, pid: i32) void {
        self.alive = true;
        self.pid = pid;
        const allocator = self.allocator;
        const new_started = isoNow(allocator) catch return;
        allocator.free(self.startedAt);
        self.startedAt = new_started;
    }

    /// SetExited marks the session as dead with the given exit code.
    pub fn setExited(self: *State, exitCode: i32) void {
        self.alive = false;
        self.exitCode = exitCode;
        const allocator = self.allocator;
        const new_exited = isoNow(allocator) catch return;
        allocator.free(self.exitedAt);
        self.exitedAt = new_exited;
        self.emit(.{ .type_ = "exit", .data = .{ .exit = .{ .exitCode = exitCode } } });
    }

    /// SetUnread marks the session as having unseen output.
    pub fn setUnread(self: *State, unread: bool) void {
        if (self.unread == unread) return;
        self.unread = unread;
        self.emit(.{ .type_ = "meta", .data = .{ .meta = .{ .unread = unread } } });
    }

    /// EmitActivity sends a lightweight "activity" event. Throttled to at most once per 2s.
    pub fn emitActivity(self: *State) void {
        const now = std.time.milliTimestamp();
        if (now - self.lastActivity < 2000) return;
        self.lastActivity = now;
        self.emit(.{ .type_ = "activity", .data = .{ .activity = {} } });
    }

    /// SetStatus updates the application status.
    pub fn setStatus(self: *State, status: ?adapter.Status) void {
        self.status = status;
        if (status) |s| {
            self.emit(.{ .type_ = "status", .data = .{ .status = s } });
        }
    }

    /// SetAdapterTitle sets the high-priority title from the adapter.
    pub fn setAdapterTitle(self: *State, new_title: []const u8) void {
        const allocator = self.allocator;
        if (std.mem.eql(u8, self.adapterTitle, new_title)) return;
        allocator.free(self.adapterTitle);
        self.adapterTitle = allocator.dupe(u8, new_title) catch new_title;
        self.emitMeta();
    }

    /// SetShellTitle sets the terminal/OSC title.
    pub fn setShellTitle(self: *State, new_title: []const u8) void {
        const allocator = self.allocator;
        if (std.mem.eql(u8, self.shellTitle, new_title)) return;
        allocator.free(self.shellTitle);
        self.shellTitle = allocator.dupe(u8, new_title) catch new_title;
        self.emitMeta();
    }

    /// SetSlug sets the URL-safe session identifier.
    pub fn setSlug(self: *State, slug: []const u8) void {
        const allocator = self.allocator;
        if (std.mem.eql(u8, self.slug, slug)) return;
        allocator.free(self.slug);
        self.slug = allocator.dupe(u8, slug) catch slug;
        self.emitMeta();
    }

    /// SetSubtitle updates the display subtitle.
    pub fn setSubtitle(self: *State, subtitle: []const u8) void {
        const allocator = self.allocator;
        allocator.free(self.subtitle);
        self.subtitle = allocator.dupe(u8, subtitle) catch subtitle;
        self.emitMeta();
    }

    /// SetTerminalSize records the current PTY dimensions.
    pub fn setTerminalSize(self: *State, cols: u16, rows: u16) void {
        self.terminalCols = cols;
        self.terminalRows = rows;
        self.emit(.{ .type_ = "terminal_resize", .data = .{ .terminalResize = .{ .cols = cols, .rows = rows } } });
    }

    fn emitMeta(self: *State) void {
        const t = self.title();
        self.emit(.{ .type_ = "meta", .data = .{ .meta = .{
            .title = t,
            .shellTitle = self.shellTitle,
            .adapterTitle = self.adapterTitle,
            .subtitle = self.subtitle,
        } } });
    }

    /// Subscribe adds a callback for events.
    pub fn subscribe(self: *State, callback: SubscriberFn) !void {
        try self.subscribers.append(callback);
    }

    fn emit(self: *State, e: Event) void {
        for (self.subscribers.items) |subscriber| {
            subscriber(e);
        }
    }

    fn isoNow(allocator: Allocator) ![]const u8 {
        const now = std.time.milliTimestamp();
        const t = std.time.fromTimestamp(@as(i64, @intCast(now / 1000))) catch return allocator.dupe(u8, "");
        return std.fmt.allocPrint(allocator, "{d:04}-{:02}-{:02}T{:02}:{:02}:{:02}Z", .{
            t.year, t.month.int(), t.day, t.hour, t.min, t.sec,
        });
    }

    fn dupSlice(allocator: Allocator, slice: []const []const u8) ![]const []const u8 {
        var result = try allocator.alloc([]const u8, slice.len);
        for (slice, 0..) |s, i| {
            result[i] = try allocator.dupe(u8, s);
        }
        return result;
    }
};

test "session state" {
    const allocator = testing.allocator;
    const cfg = Config{
        .id = "test-123",
        .command = &.{"echo", "hello"},
        .cwd = "/tmp",
        .kind = "shell",
        .socketPath = "/tmp/test.sock",
    };
    var state = try State.init(allocator, cfg);
    defer state.deinit();

    state.setRunning(12345);
    try std.testing.expect(state.alive);
    try std.testing.expectEqual(@as(i32, 12345), state.pid);

    state.setExited(0);
    try std.testing.expect(!state.alive);
    try std.testing.expectEqual(@as(?i32, 0), state.exitCode);
}

const testing = std.testing;

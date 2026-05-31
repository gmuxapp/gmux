// Package subscribe handles SSE subscriptions to runners.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// Subscriptions manages SSE subscriptions to runners.
pub const Subscriptions = struct {
    allocator: Allocator,
    sessions: *store.Store,
    onDead: ?*const fn (session: *store.Session) callconv(.C) void,
    onExit: ?*const fn (session: *store.Session) callconv(.C) bool,

    pub fn init(allocator: Allocator, sessions: *store.Store) !*Subscriptions {
        const self = try allocator.create(Subscriptions);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.sessions = sessions;
        self.onDead = null;
        self.onExit = null;

        return self;
    }

    pub fn deinit(self: *Subscriptions) void {
        self.allocator.destroy(self);
    }

    /// subscribe subscribes to a runner's /events SSE.
    pub fn subscribe(self: *Subscriptions, socketPath: []const u8) !void {
        _ = self;
        _ = socketPath;
        // In real implementation, this would connect to the SSE endpoint
    }

    /// unsubscribe unsubscribes from a runner.
    pub fn unsubscribe(self: *Subscriptions, sessionId: []const u8) void {
        _ = self;
        _ = sessionId;
        // In real implementation, this would close the SSE connection
    }
};

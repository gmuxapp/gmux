// Package notify provides notification routing.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// Router routes notifications to the appropriate clients.
pub const Router = struct {
    allocator: Allocator,
    sessions: *store.Store,
    config: Config,

    pub const Config = struct {
        timeout: std.time.Duration = 5000,
    };

    pub fn init(allocator: Allocator, sessions: *store.Store, config: Config) !*Router {
        const self = try allocator.create(Router);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.sessions = sessions;
        self.config = config;

        return self;
    }

    pub fn deinit(self: *Router) void {
        self.allocator.destroy(self);
    }

    /// run starts the notification router.
    pub fn run(self: *Router) void {
        _ = self;
        // In real implementation, this would monitor sessions and route notifications
    }

    /// cancelAllPending cancels all pending notifications.
    pub fn cancelAllPending(self: *Router) void {
        _ = self;
    }

    /// cancelForSession cancels notifications for a specific session.
    pub fn cancelForSession(self: *Router, session_id: []const u8) void {
        _ = self;
        _ = session_id;
    }
};

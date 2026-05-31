// Package filemon monitors adapter session files.
const std = @import("std");
const Allocator = std.mem.Allocator;
const store = @import("../store/store.zig");

/// FileMonitor monitors adapter session files.
pub const FileMonitor = struct {
    allocator: Allocator,
    sessions: *store.Store,

    pub fn init(allocator: Allocator, sessions: *store.Store) !*FileMonitor {
        const self = try allocator.create(FileMonitor);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.sessions = sessions;

        return self;
    }

    pub fn deinit(self: *FileMonitor) void {
        self.allocator.destroy(self);
    }

    /// run starts monitoring files.
    pub fn run(self: *FileMonitor) !void {
        _ = self;
        // In real implementation, this would use inotify to watch files
    }

    /// resolveResumeCommand resolves the resume command for a session.
    pub fn resolveResumeCommand(self: *FileMonitor, session: *store.Session) ?[]const []const u8 {
        _ = self;
        _ = session;
        return null;
    }
};

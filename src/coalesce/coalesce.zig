// Package coalesce provides coalescing for SSE events.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Coalescer buffers events and emits them in batches.
pub const Coalescer = struct {
    allocator: Allocator,
    interval: std.time.Duration,
    max_items: usize,
    events: std.ArrayList([]const u8),
    timer: ?std.time.Timer,

    pub fn init(allocator: Allocator, interval: std.time.Duration, max_items: usize) !*Coalescer {
        const self = try allocator.create(Coalescer);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.interval = interval;
        self.max_items = max_items;
        self.events = std.ArrayList([]const u8).init(allocator);
        self.timer = null;

        return self;
    }

    pub fn deinit(self: *Coalescer) void {
        for (self.events.items) |e| {
            self.allocator.free(e);
        }
        self.events.deinit();
        self.allocator.destroy(self);
    }

    /// add adds an event to the coalescer.
    pub fn add(self: *Coalescer, event: []const u8) !void {
        const duped = try self.allocator.dupe(u8, event);
        try self.events.append(duped);

        if (self.events.items.len >= self.max_items) {
            try self.flush();
        }
    }

    /// flush emits all buffered events.
    pub fn flush(self: *Coalescer) !void {
        if (self.events.items.len == 0) return;

        // Emit all events
        for (self.events.items) |event| {
            // In real implementation, this would write to SSE stream
            _ = event;
        }

        for (self.events.items) |event| {
            self.allocator.free(event);
        }
        self.events.clearRetainingCapacity();
    }
};

test "coalescer add and flush" {
    var c = try Coalescer.init(std.testing.allocator, 1000, 10);
    defer c.deinit();

    try c.add("event1");
    try c.add("event2");
    try std.testing.expectEqual(@as(usize, 2), c.events.items.len);

    try c.flush();
    try std.testing.expectEqual(@as(usize, 0), c.events.items.len);
}

test "coalescer auto-flush at max" {
    var c = try Coalescer.init(std.testing.allocator, 1000, 2);
    defer c.deinit();

    try c.add("event1");
    try std.testing.expectEqual(@as(usize, 1), c.events.items.len);

    // Adding a second event should trigger auto-flush
    try c.add("event2");
    try std.testing.expectEqual(@as(usize, 0), c.events.items.len);
}

test "coalescer flush empty" {
    var c = try Coalescer.init(std.testing.allocator, 1000, 10);
    defer c.deinit();

    // Should not crash on empty flush
    try c.flush();
}

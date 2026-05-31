// Package presence tracks client presence.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Callbacks for presence events.
pub const Callbacks = struct {
    onClientFocused: ?*const fn (client_id: []const u8) callconv(.C) void = null,
    onSessionSelected: ?*const fn (client_id: []const u8, session_id: []const u8) callconv(.C) void = null,
};

/// Table tracks client presence state.
pub const Table = struct {
    allocator: Allocator,
    callbacks: Callbacks,
    clients: std.StringHashMap(ClientState),

    const ClientState = struct {
        focused: bool = false,
        selectedSession: []const u8 = "",
    };

    pub fn init(allocator: Allocator, callbacks: Callbacks) !*Table {
        const self = try allocator.create(Table);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.callbacks = callbacks;
        self.clients = std.StringHashMap(ClientState).init(allocator);

        return self;
    }

    pub fn deinit(self: *Table) void {
        var it = self.clients.iterator();
        while (it.next()) |entry| {
            self.allocator.free(entry.value_ptr.*.selectedSession);
        }
        self.clients.deinit();
        self.allocator.destroy(self);
    }

    /// focus marks a client as focused.
    pub fn focus(self: *Table, client_id: []const u8) void {
        if (self.clients.get(client_id)) |*state| {
            state.focused = true;
            if (self.callbacks.onClientFocused) |cb| {
                cb(client_id);
            }
        }
    }

    /// selectSession marks a session as selected by a client.
    pub fn selectSession(self: *Table, client_id: []const u8, session_id: []const u8) void {
        if (self.clients.get(client_id)) |*state| {
            self.allocator.free(state.selectedSession);
            state.selectedSession = self.allocator.dupe(u8, session_id) catch session_id;
            if (self.callbacks.onSessionSelected) |cb| {
                cb(client_id, session_id);
            }
        }
    }

    /// put adds a client.
    pub fn put(self: *Table, client_id: []const u8) !void {
        const state = ClientState{};
        try self.clients.put(client_id, state);
    }
};

test "presence table focus" {
    var t = try Table.init(std.testing.allocator, Callbacks{});
    defer t.deinit();

    try t.put("client1");
    t.focus("client1");

    const state = t.clients.get("client1").?;
    try std.testing.expect(state.focused);
}

test "presence table selectSession" {
    var t = try Table.init(std.testing.allocator, Callbacks{});
    defer t.deinit();

    try t.put("client1");
    t.selectSession("client1", "session1");

    const state = t.clients.get("client1").?;
    try std.testing.expectEqualStrings("session1", state.selectedSession);
}

test "presence table no-op on unknown client" {
    var t = try Table.init(std.testing.allocator, Callbacks{});
    defer t.deinit();

    // Should not crash
    t.focus("unknown");
    t.selectSession("unknown", "session1");
}

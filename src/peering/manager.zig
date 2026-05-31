// Package peering manages peer connections.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Peer represents a connected peer.
pub const Peer = struct {
    name: []const u8,
    addr: []const u8,
    local: bool,
};

/// Manager manages peer connections.
pub const Manager = struct {
    allocator: Allocator,
    peers: std.StringHashMap(Peer),

    pub fn init(allocator: Allocator) !*Manager {
        const self = try allocator.create(Manager);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.peers = std.StringHashMap(Peer).init(allocator);

        return self;
    }

    pub fn deinit(self: *Manager) void {
        var it = self.peers.iterator();
        while (it.next()) |entry| {
            self.allocator.free(entry.value_ptr.*.name);
            self.allocator.free(entry.value_ptr.*.addr);
        }
        self.peers.deinit();
        self.allocator.destroy(self);
    }

    /// add adds a peer.
    pub fn add(self: *Manager, name: []const u8, addr: []const u8, local: bool) !void {
        const peer = Peer{
            .name = try self.allocator.dupe(u8, name),
            .addr = try self.allocator.dupe(u8, addr),
            .local = local,
        };
        try self.peers.put(name, peer);
    }

    /// get retrieves a peer by name.
    pub fn get(self: *Manager, name: []const u8) ?Peer {
        return self.peers.get(name);
    }

    /// isLocalPeer checks if a peer is a local peer (devcontainer).
    pub fn isLocalPeer(self: *Manager, name: []const u8) bool {
        if (self.peers.get(name)) |peer| {
            return peer.local;
        }
        return false;
    }
};

test "peering manager add and get" {
    var m = try Manager.init(std.testing.allocator);
    defer m.deinit();

    try m.add("peer1", "127.0.0.1:8080", false);
    try m.add("local-peer", "127.0.0.2:8080", true);

    const p1 = m.get("peer1").?;
    try std.testing.expectEqualStrings("peer1", p1.name);
    try std.testing.expectEqualStrings("127.0.0.1:8080", p1.addr);
    try std.testing.expect(!p1.local);

    const p2 = m.get("local-peer").?;
    try std.testing.expect(p2.local);

    try std.testing.expectEqual(false, m.get("nonexistent"));
}

test "peering manager isLocalPeer" {
    var m = try Manager.init(std.testing.allocator);
    defer m.deinit();

    try m.add("local", "127.0.0.1:8080", true);
    try m.add("remote", "10.0.0.1:8080", false);

    try std.testing.expect(m.isLocalPeer("local"));
    try std.testing.expect(!m.isLocalPeer("remote"));
    try std.testing.expect(!m.isLocalPeer("unknown"));
}

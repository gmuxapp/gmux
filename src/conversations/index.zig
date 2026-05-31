// Package conversations indexes conversation files.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Index indexes conversation files by (kind, slug).
pub const Index = struct {
    allocator: Allocator,
    entries: std.StringHashMap(Entry),

    const Entry = struct {
        slug: []const u8,
        kind: []const u8,
        title: []const u8,
        cwd: []const u8,
        resumeCommand: []const []const u8,
        created: i64,
    };

    pub fn init(allocator: Allocator) !*Index {
        const self = try allocator.create(Index);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.entries = std.StringHashMap(Entry).init(allocator);

        return self;
    }

    pub fn deinit(self: *Index) void {
        var it = self.entries.iterator();
        while (it.next()) |entry| {
            self.allocator.free(entry.value_ptr.*.slug);
            self.allocator.free(entry.value_ptr.*.kind);
            self.allocator.free(entry.value_ptr.*.title);
            self.allocator.free(entry.value_ptr.*.cwd);
            for (entry.value_ptr.*.resumeCommand) |c| {
                self.allocator.free(c);
            }
            self.allocator.free(entry.value_ptr.*.resumeCommand);
        }
        self.entries.deinit();
        self.allocator.destroy(self);
    }

    /// lookup looks up a conversation by (kind, slug).
    pub fn lookup(self: *Index, kind: []const u8, slug: []const u8) ?Entry {
        const key = kind ++ "/" ++ slug;
        return self.entries.get(key);
    }

    /// put adds an entry.
    pub fn put(self: *Index, kind: []const u8, slug: []const u8, entry: Entry) !void {
        const key = kind ++ "/" ++ slug;
        try self.entries.put(key, entry);
    }
};

test "conversations index lookup" {
    var idx = try Index.init(std.testing.allocator);
    defer idx.deinit();

    const entry = Index.Entry{
        .slug = "my-slug",
        .kind = "pi",
        .title = "My Conversation",
        .cwd = "/home/user/project",
        .resumeCommand = &.{ "pi", "resume" },
        .created = 1234567890,
    };
    try idx.put("pi", "my-slug", entry);

    const found = idx.lookup("pi", "my-slug").?;
    try std.testing.expectEqualStrings("my-slug", found.slug);
    try std.testing.expectEqualStrings("pi", found.kind);
    try std.testing.expectEqualStrings("My Conversation", found.title);

    try std.testing.expect(idx.lookup("pi", "nonexistent") == null);
}

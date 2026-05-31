// Package projects manages project configuration and session assignment.
const std = @import("std");
const Allocator = std.mem.Allocator;
const paths = @import("../paths/paths.zig");

/// Item represents a configured project.
pub const Item = struct {
    slug: []const u8,
    match_: []const MatchRule,
    sessions: []const SessionRef,

    pub const MatchRule = struct {
        path: []const u8 = "",
        remote: []const u8 = "",
    };

    pub const SessionRef = struct {
        id: []const u8,
        slug: []const u8 = "",
    };
};

/// State is the projects.json state.
pub const State = struct {
    items: []const Item,
    version: u32 = 1,

    pub fn init(allocator: Allocator) State {
        _ = allocator;
        return .{
            .items = &.{},
        };
    }

    pub fn deinit(self: *State, allocator: Allocator) void {
        for (self.items) |item| {
            allocator.free(item.slug);
            for (item.match_) |rule| {
                allocator.free(rule.path);
                allocator.free(rule.remote);
            }
            allocator.free(item.match_);
            for (item.sessions) |ref| {
                allocator.free(ref.id);
                allocator.free(ref.slug);
            }
            allocator.free(item.sessions);
        }
        allocator.free(self.items);
    }

    pub fn validate(self: *State, allocator: Allocator) !void {
        var seen = std.AutoHashMap([]const u8, void).init(allocator);
        defer seen.deinit();

        for (self.items) |item| {
            if (seen.get(item.slug)) |_| {
                return error.DuplicateSlug;
            }
            try seen.put(item.slug, {});
        }
    }
};

/// Manager handles concurrent access to projects.json.
pub const Manager = struct {
    allocator: Allocator,
    dir: []const u8,
    state: State,

    pub fn init(allocator: Allocator, dir: []const u8) !*Manager {
        const self = try allocator.create(Manager);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.dir = try allocator.dupe(u8, dir);
        self.state = State.init(allocator);

        return self;
    }

    pub fn deinit(self: *Manager) void {
        self.state.deinit(self.allocator);
        self.allocator.free(self.dir);
        self.allocator.destroy(self);
    }

    pub fn load(self: *Manager) !State {
        const path = try std.fs.path.join(self.allocator, &.{ self.dir, "projects.json" });
        defer self.allocator.free(path);

        const file = std.fs.openFileAbsolute(path, .{}) catch |err| {
            if (err == error.FileNotFound) return State.init(self.allocator);
            return err;
        };
        defer file.close();

        const content = try file.readToEndAlloc(self.allocator, 65536);
        defer self.allocator.free(content);




        return self.state;
    }
};

/// NormalizePath expands ~ to $HOME.
pub fn normalizePath(allocator: Allocator, p: []const u8) ![]const u8 {
    return paths.normalizePath(allocator, p);
}

/// SlugFromRemote derives a project slug from a git remote URL.
pub fn slugFromRemote(remote: []const u8) []const u8 {
    var it = std.mem.splitScalar(u8, remote, '/');
    var last: []const u8 = "";
    while (it.next()) |part| {
        last = part;
    }
    var name = last;
    if (std.mem.endsWith(u8, name, ".git")) {
        name = name[0 .. name.len - 4];
    }
    return name;
}

/// SlugFromPath derives a project slug from a filesystem path.
pub fn slugFromPath(path: []const u8) []const u8 {
    return std.fs.path.basename(path);
}

/// UniqueSlug ensures the slug is unique among existing items.
pub fn uniqueSlug(allocator: Allocator, slug: []const u8, items: []const Item) []const u8 {
    var candidate = slug;
    var counter: u32 = 1;

    while (true) {
        var found = false;
        for (items) |item| {
            if (std.mem.eql(u8, item.slug, candidate)) {
                found = true;
                break;
            }
        }
        if (!found) return candidate;
        candidate = std.fmt.allocPrint(allocator, "{s}-{d}", .{ slug, counter }) catch slug;
        counter += 1;
    }
}

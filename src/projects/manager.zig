// Package projects provides project management.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Manager manages projects.
pub const Manager = struct {
    allocator: Allocator,
    projects: std.StringHashMap(Project),

    pub fn init(allocator: Allocator) !*Manager {
        const self = try allocator.create(Manager);
        errdefer allocator.destroy(self);

        self.allocator = allocator;
        self.projects = std.StringHashMap(Project).init(allocator);

        return self;
    }

    pub fn deinit(self: *Manager) void {
        var it = self.projects.iterator();
        while (it.next()) |entry| {
            self.allocator.free(entry.value_ptr.*.path);
            self.allocator.free(entry.value_ptr.*.name);
        }
        self.projects.deinit();
        self.allocator.destroy(self);
    }

    /// add adds a project.
    pub fn add(self: *Manager, name: []const u8, path: []const u8) !void {
        const project = Project{
            .name = try self.allocator.dupe(u8, name),
            .path = try self.allocator.dupe(u8, path),
        };
        try self.projects.put(name, project);
    }

    /// get retrieves a project by name.
    pub fn get(self: *Manager, name: []const u8) ?Project {
        return self.projects.get(name);
    }
};

/// Project represents a project.
pub const Project = struct {
    name: []const u8,
    path: []const u8,
};

// Package localterm provides transparent local terminal attach for gmux.
const std = @import("std");
const Allocator = std.mem.Allocator;

/// Attach represents a local terminal attachment to a PTY session.
pub const Attach = struct {
    stdin: std.posix_fd,
    stdout: std.posix_fd,
    ptyWriter: *PTYWriter,
    resizeFn: *const fn (cols: u16, rows: u16) callconv(.C) void,
    detached: bool,
    done: bool,

    const posix_fd = c_int;

    pub fn isInteractive() bool {
        return std.posix.isatty(std.posix.stdin);
    }

    pub fn terminalSize() struct { cols: u16, rows: u16 } {
        var winsize: std.posix.winsize = undefined;
        if (std.posix.ioctl(std.posix.stdin, std.posix.TIOCGWINSZ, &winsize) == 0) {
            return .{ .cols = winsize.cols, .rows = winsize.rows };
        }
        return .{ .cols = 80, .rows = 24 };
    }

    pub fn init(ptyWriter: *PTYWriter, resizeFn: *const fn (cols: u16, rows: u16) callconv(.C) void) !*Attach {
        const self = try std.heap.c_allocator.create(Attach);
        self.stdin = std.posix.stdin;
        self.stdout = std.posix.stdout;
        self.ptyWriter = ptyWriter;
        self.resizeFn = resizeFn;
        self.detached = false;
        self.done = false;

        // Enter raw mode
        try setRawMode(self.stdin);

        return self;
    }

    pub fn deinit(self: *Attach) void {
        if (!self.detached) {
            self.detach();
        }
        std.heap.c_allocator.destroy(self);
    }

    pub fn start(self: *Attach) void {
        // Start reading from stdin
        var buf: [4096]u8 = undefined;
        while (!self.detached) {
            const n = std.posix.read(self.stdin, &buf) catch break;
            if (n > 0) {
                self.ptyWriter.write(buf[0..n]);
            }
        }
    }

    pub fn write(self: *Attach, data: []const u8) void {
        if (self.detached) return;
        std.posix.writeAll(self.stdout, data) catch {};
    }

    pub fn detach(self: *Attach) void {
        if (self.detached) return;
        self.detached = true;
        restoreTerminal(self.stdin);
    }

    fn setRawMode(fd: posix_fd) !void {
        var termios: std.posix.termios = undefined;
        try std.posix.tcgetattr(fd, &termios);

        // Input modes: disable canonical mode, echo, etc.
        termios.iflag &= ~(@as(u32, std.posix.IGNBRK) | std.posix.BRKINT | std.posix.INLCR | std.posix.IGNCR | std.posix.ICANON | std.posix.ECHO | std.posix.ECHOE | std.posix.ECHOK | std.posix.ECHONL | std.posix.NOFLSH);
        termios.lflag &= ~(@as(u32, std.posix.ICANON) | std.posix.ECHO | std.posix.ECHOE | std.posix.ECHOK | std.posix.ECHONL | std.posix.ISIG | std.posix.IEXTEN);

        try std.posix.tcsetattr(fd, .now, &termios);
    }

    fn restoreTerminal(fd: posix_fd) void {
        var termios: std.posix.termios = undefined;
        std.posix.tcgetattr(fd, &termios) catch return;
        std.posix.tcsetattr(fd, .now, &termios) catch {};
    }
};

/// PTYWriter is a writer for PTY data.
pub const PTYWriter = struct {
    fd: std.posix_fd,

    pub fn init(fd: std.posix_fd) PTYWriter {
        return .{ .fd = fd };
    }

    pub fn write(self: *PTYWriter, data: []const u8) void {
        std.posix.writeAll(self.fd, data) catch {};
    }
};

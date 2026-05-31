// Package adapters contains all adapter implementations.
const shell = @import("../adapters/shell.zig");
const attribution = @import("attribution.zig");

pub const Shell = shell.Shell;
pub const Attribution = attribution.Attribution;

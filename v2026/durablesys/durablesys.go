// Custody syscalls whose kernel interfaces differ between Linux and Darwin.
// Every primitive has the same observable contract on both platforms or
// refuses: renames never fall back to an overwriting rename, attribute writes
// never drop their create/replace condition, and an absent attribute is always
// reported as ErrNoAttribute. Durability is not wrapped here; os.File.Sync is
// fsync on Linux and F_FULLFSYNC on Darwin.
//
// Functions accept raw descriptors and are safe for concurrent use. Callers
// keep the owning *os.File alive across each call.
package durablesys

// Package dto holds the Jellyfin 12.1.0 wire types (DESIGN §3.3).
//
// types.gen.go is generated from openapi.json by scripts/gen-dto.py (`make dto`);
// never edit it by hand. The hand-written helpers here define how the special
// formats go over the wire:
//
//   - ID:    GUIDs as 32 lower-case hex digits ("N" format); input also accepts dashed.
//   - Time:  UTC, 100 ns precision; non-zero fractions trimmed (".5Z"), zero ones as ".0000000Z".
//   - Ticks: durations and positions in 100 ns units.
package dto

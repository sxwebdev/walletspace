package raydium

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/gagliardetto/solana-go"
)

// Tick arrays (raydium-clmm, states/tick_array.rs and
// states/tickarray_bitmap_extension.rs, libraries/tick_array_bit_map.rs).
const (
	minTick = -443636 // libraries/tick_math.rs MIN_TICK
	maxTick = 443636  // libraries/tick_math.rs MAX_TICK

	// ticksPerArray is TICK_ARRAY_SIZE; a tick array spans
	// ticksPerArray × tick_spacing ticks.
	ticksPerArray = 60
	// arraysPerBitmap is TICK_ARRAY_BITMAP_SIZE: the default bitmap in the
	// pool covers ±arraysPerBitmap arrays around tick 0, each extension
	// bitmap another arraysPerBitmap arrays.
	arraysPerBitmap  = 512
	extensionBitmaps = 14 // EXTENSION_TICKARRAY_BITMAP_SIZE

	tickArraySize           = 10240
	tickArrayPoolOffset     = 8
	tickArrayStartOffset    = 40
	tickArrayCountOffset    = 10124
	bitmapExtensionSize     = 1832
	bitmapExtensionPool     = 8
	bitmapExtensionPositive = 40
	bitmapExtensionNegative = 40 + 64*extensionBitmaps

	tickArraySeed       = "tick_array"
	bitmapExtensionSeed = "pool_tick_array_bitmap_extension"
)

var (
	tickArrayDiscriminator       = mustHex("c09b55cd31f9812a")
	bitmapExtensionDiscriminator = mustHex("3c9624db61808b99")

	// ErrTickArrays means the tick arrays of a CLMM swap are not the ones
	// the program walks for this pool and direction.
	ErrTickArrays = errors.New("raydium: tick arrays do not match the pool")
	// ErrNeedBitmapExtension means the walk leaves the range of the pool's
	// own bitmap and needs the bitmap extension account's data.
	ErrNeedBitmapExtension = errors.New("raydium: the pool's bitmap extension is needed")
)

// CLMMTickArrayAddress is the tick array PDA: ["tick_array", pool, start
// index as a big-endian i32] under the CLMM program.
func CLMMTickArrayAddress(pool solana.PublicKey, start int32) solana.PublicKey {
	return pda([][]byte{[]byte(tickArraySeed), pool[:], binary.BigEndian.AppendUint32(nil, uint32(start))}, CLMMProgram)
}

// CLMMBitmapExtensionAddress is the PDA of
// ["pool_tick_array_bitmap_extension", pool] under the CLMM program.
func CLMMBitmapExtensionAddress(pool solana.PublicKey) solana.PublicKey {
	return pda([][]byte{[]byte(bitmapExtensionSeed), pool[:]}, CLMMProgram)
}

// CLMMTickArray is the header of a tick array account.
type CLMMTickArray struct {
	Pool                 solana.PublicKey
	StartTickIndex       int32
	InitializedTickCount uint8
}

// DecodeCLMMTickArray decodes the header of a tick array account owned by
// owner.
func DecodeCLMMTickArray(owner solana.PublicKey, data []byte) (CLMMTickArray, error) {
	if err := checkAccount("CLMM tick array", CLMMProgram, owner, data, tickArraySize, tickArrayDiscriminator); err != nil {
		return CLMMTickArray{}, err
	}
	l := layout(data)
	return CLMMTickArray{
		Pool:                 l.key(tickArrayPoolOffset),
		StartTickIndex:       l.i32(tickArrayStartOffset),
		InitializedTickCount: l.u8(tickArrayCountOffset),
	}, nil
}

// CLMMBitmapExtension holds the initialized-array bits of a pool beyond the
// range of its own bitmap.
type CLMMBitmapExtension struct {
	Pool     solana.PublicKey
	Positive [extensionBitmaps][8]uint64
	Negative [extensionBitmaps][8]uint64
}

// DecodeCLMMBitmapExtension decodes a bitmap extension account owned by
// owner.
func DecodeCLMMBitmapExtension(owner solana.PublicKey, data []byte) (CLMMBitmapExtension, error) {
	if err := checkAccount("CLMM bitmap extension", CLMMProgram, owner, data, bitmapExtensionSize, bitmapExtensionDiscriminator); err != nil {
		return CLMMBitmapExtension{}, err
	}
	l := layout(data)
	out := CLMMBitmapExtension{Pool: l.key(bitmapExtensionPool)}
	for i := range extensionBitmaps {
		for j := range 8 {
			out.Positive[i][j] = l.u64(bitmapExtensionPositive + 64*i + 8*j)
			out.Negative[i][j] = l.u64(bitmapExtensionNegative + 64*i + 8*j)
		}
	}
	return out, nil
}

// arrayStart is TickArrayState::get_array_start_index: the start of the
// array holding tick, rounding towards negative infinity.
func arrayStart(tick int32, spacing uint16) int32 {
	width := int32(ticksPerArray) * int32(spacing)
	start := tick / width
	if tick < 0 && tick%width != 0 {
		start--
	}
	return start * width
}

// initialized reports whether the tick array starting at start is marked
// initialized: in the pool's bitmap within ±512 arrays of zero
// (check_current_tick_array_is_initialized: bit start/width + 512 of a
// little-endian U1024), beyond that in the extension
// (TickArrayBitmapExtension::get_bitmap and tick_array_offset_in_bitmap).
func (p CLMMPool) initialized(start int32, extension *CLMMBitmapExtension) (bool, error) {
	width := int64(ticksPerArray) * int64(p.TickSpacing)
	boundary := width * arraysPerBitmap
	s := int64(start)
	if s >= -boundary && s < boundary {
		bit := s/width + arraysPerBitmap
		return p.TickArrayBitmap[bit/64]>>(bit%64)&1 == 1, nil
	}
	if extension == nil {
		return false, ErrNeedBitmapExtension
	}
	// Callers stay within the starts that can hold MIN_TICK..MAX_TICK, so
	// k is at most 13: MAX_TICK < 15 × 30720, the extension's reach at tick
	// spacing 1, and wider spacings reach further with the same 14 bitmaps.
	var bitmap [8]uint64
	var bit int64
	if s > 0 {
		k := s/boundary - 1
		bitmap, bit = extension.Positive[k], (s-(k+1)*boundary)/width
	} else {
		k := -s/boundary - 1
		if -s%boundary == 0 {
			k--
		}
		bitmap, bit = extension.Negative[k], (s+(k+2)*boundary)/width
	}
	return bitmap[bit/64]>>(bit%64)&1 == 1, nil
}

// TickArrayStarts returns the start indexes of the first n tick arrays a
// swap in the given direction walks, as the program finds them
// (pool.rs first_tick_array_index_with_extension_info, then
// next_tick_array_index_with_extension_info): the array holding the current
// tick if it is initialized, otherwise the next initialized one in the
// direction, then each next initialized array after it. It returns fewer than
// n when the direction runs out of initialized arrays, and
// ErrNeedBitmapExtension when the walk leaves the pool's own bitmap and
// extension is nil.
func (p CLMMPool) TickArrayStarts(extension *CLMMBitmapExtension, zeroForOne bool, n int) ([]int32, error) {
	if p.TickSpacing == 0 {
		return nil, fmt.Errorf("%w: tick spacing 0", ErrMalformedAccount)
	}
	width := int32(ticksPerArray) * int32(p.TickSpacing)
	lowest, highest := arrayStart(minTick, p.TickSpacing), arrayStart(maxTick, p.TickSpacing)
	step := width
	if zeroForOne {
		step = -width
	}
	var out []int32
	for start := arrayStart(p.TickCurrent, p.TickSpacing); len(out) < n && start >= lowest && start <= highest; start += step {
		ok, err := p.initialized(start, extension)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, start)
		}
	}
	return out, nil
}

// TickArrays returns the addresses of the first n tick arrays a swap in the
// given direction walks; see TickArrayStarts.
func (p CLMMPool) TickArrays(pool solana.PublicKey, extension *CLMMBitmapExtension, zeroForOne bool, n int) ([]solana.PublicKey, error) {
	starts, err := p.TickArrayStarts(extension, zeroForOne, n)
	if err != nil {
		return nil, err
	}
	out := make([]solana.PublicKey, len(starts))
	for i, start := range starts {
		out[i] = CLMMTickArrayAddress(pool, start)
	}
	return out, nil
}

// CheckTickArrays checks tick arrays someone else chose — the Trade API's
// remainingAccounts — against the walk the program makes: they must be
// exactly the first len(given) arrays of TickArrays, in order. An extension
// passed here must belong to the pool.
func (p CLMMPool) CheckTickArrays(pool solana.PublicKey, extension *CLMMBitmapExtension, zeroForOne bool, given []solana.PublicKey) error {
	if len(given) == 0 || len(given) > MaxTickArrays {
		return fmt.Errorf("%w: %d tick arrays, want 1..%d", ErrTickArrays, len(given), MaxTickArrays)
	}
	if extension != nil && extension.Pool != pool {
		return fmt.Errorf("%w: the bitmap extension belongs to pool %s", ErrTickArrays, extension.Pool)
	}
	want, err := p.TickArrays(pool, extension, zeroForOne, len(given))
	if err != nil {
		return err
	}
	if !slices.Equal(want, given) {
		return fmt.Errorf("%w: got %v, the walk from tick %d gives %v", ErrTickArrays, given, p.TickCurrent, want)
	}
	return nil
}

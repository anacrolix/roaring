package roaring64

import (
	"fmt"
	"io"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	// Min64BitSigned - Minimum 64 bit value
	Min64BitSigned = -9223372036854775808
	// Max64BitSigned - Maximum 64 bit value
	Max64BitSigned = 9223372036854775807
)

// BSI is at its simplest is an array of bitmaps that represent an encoded
// binary value.  The advantage of a BSI is that comparisons can be made
// across ranges of values whereas a bitmap can only represent the existence
// of a single value for a given column ID.  Another usage scenario involves
// storage of high cardinality values.
//
// It depends upon the bitmap libraries.  It is not thread safe, so
// upstream concurrency guards must be provided.
type BSI struct {
	bA           []Bitmap
	eBM          Bitmap // Existence BitMap
	MaxValue     int64
	MinValue     int64
	runOptimized bool
}

// NewBSI constructs a new BSI. Note that it is your responsibility to ensure that
// the min/max values are set correctly. Queries CompareValue, MinMax, etc. will not
// work correctly if the min/max values are not set correctly.
func NewBSI(maxValue int64, minValue int64) *BSI {

	bitsz := bits.Len64(uint64(minValue))
	if bits.Len64(uint64(maxValue)) > bitsz {
		bitsz = bits.Len64(uint64(maxValue))
	}
	ba := make([]Bitmap, bitsz)
	return &BSI{bA: ba, MaxValue: maxValue, MinValue: minValue}
}

// NewDefaultBSI constructs an auto-sized BSI
func NewDefaultBSI() *BSI {
	return NewBSI(int64(0), int64(0))
}

// RunOptimize attempts to further compress the runs of consecutive values found in the bitmap
func (b *BSI) RunOptimize() {
	b.eBM.RunOptimize()
	for i := 0; i < len(b.bA); i++ {
		b.bA[i].RunOptimize()
	}
	b.runOptimized = true
}

// HasRunCompression returns true if the bitmap benefits from run compression
func (b *BSI) HasRunCompression() bool {
	return b.runOptimized
}

// GetExistenceBitmap returns a pointer to the underlying existence bitmap of the BSI
func (b *BSI) GetExistenceBitmap() *Bitmap {
	return &b.eBM
}

// ValueExists tests whether the value exists.
func (b *BSI) ValueExists(columnID uint64) bool {

	return b.eBM.Contains(uint64(columnID))
}

// GetCardinality returns a count of unique column IDs for which a value has been set.
func (b *BSI) GetCardinality() uint64 {
	return b.eBM.GetCardinality()
}

// BitCount returns the number of bits needed to represent values.
func (b *BSI) BitCount() int {
	return len(b.bA)
}

// SetValue sets a value for a given columnID.
func (b *BSI) SetValue(columnID uint64, value int64) {
	// If max/min values are set to zero then automatically determine bit array size
	if b.MaxValue == 0 && b.MinValue == 0 {
		minBits := bits.Len64(uint64(value))
		for len(b.bA) < minBits {
			b.bA = append(b.bA, Bitmap{})
		}
	}

	for i := 0; i < b.BitCount(); i++ {
		if uint64(value)&(1<<uint64(i)) > 0 {
			b.bA[i].Add(columnID)
		} else {
			b.bA[i].Remove(columnID)
		}
	}
	b.eBM.Add(columnID)
}

// GetValue gets the value at the column ID. Second param will be false for non-existent values.
func (b *BSI) GetValue(columnID uint64) (value int64, exists bool) {
	exists = b.eBM.Contains(columnID)
	if !exists {
		return
	}
	for i := 0; i < b.BitCount(); i++ {
		if b.bA[i].Contains(columnID) {
			value |= 1 << i
		}
	}
	return
}

type action func(t *task, batch []uint64, resultsChan chan *Bitmap, wg *sync.WaitGroup)

func parallelExecutor(parallelism int, t *task, e action, foundSet *Bitmap) *Bitmap {

	var n int = parallelism
	if n == 0 {
		n = runtime.NumCPU()
	}

	resultsChan := make(chan *Bitmap, n)

	card := foundSet.GetCardinality()
	x := card / uint64(n)

	remainder := card - (x * uint64(n))
	var batch []uint64
	var wg sync.WaitGroup
	iter := foundSet.ManyIterator()
	for i := 0; i < n; i++ {
		if i == n-1 {
			batch = make([]uint64, x+remainder)
		} else {
			batch = make([]uint64, x)
		}
		iter.NextMany(batch)
		wg.Add(1)
		go e(t, batch, resultsChan, &wg)
	}

	wg.Wait()

	close(resultsChan)

	ba := make([]*Bitmap, 0)
	for bm := range resultsChan {
		ba = append(ba, bm)
	}

	return ParOr(0, ba...)

}

type bsiAction func(input *BSI, filterSet *Bitmap, batch []uint64, resultsChan chan *BSI, wg *sync.WaitGroup)

func parallelExecutorBSIResults(parallelism int, input *BSI, e bsiAction, foundSet, filterSet *Bitmap, sumResults bool) *BSI {

	var n int = parallelism
	if n == 0 {
		n = runtime.NumCPU()
	}

	resultsChan := make(chan *BSI, n)

	card := foundSet.GetCardinality()
	x := card / uint64(n)

	remainder := card - (x * uint64(n))
	var batch []uint64
	var wg sync.WaitGroup
	iter := foundSet.ManyIterator()
	for i := 0; i < n; i++ {
		if i == n-1 {
			batch = make([]uint64, x+remainder)
		} else {
			batch = make([]uint64, x)
		}
		iter.NextMany(batch)
		wg.Add(1)
		go e(input, filterSet, batch, resultsChan, &wg)
	}

	wg.Wait()

	close(resultsChan)

	ba := make([]*BSI, 0)
	for bm := range resultsChan {
		ba = append(ba, bm)
	}

	results := NewDefaultBSI()
	if sumResults {
		for _, v := range ba {
			results.Add(v)
		}
	} else {
		results.ParOr(0, ba...)
	}
	return results

}

// Operation identifier
type Operation int

const (
	// LT less than
	LT Operation = 1 + iota
	// LE less than or equal
	LE
	// EQ equal
	EQ
	// GE greater than or equal
	GE
	// GT greater than
	GT
	// RANGE range
	RANGE
	// MIN find minimum
	MIN
	// MAX find maximum
	MAX
)

type task struct {
	bsi          *BSI
	op           Operation
	valueOrStart int64
	end          int64
	values       map[int64]struct{}
	bits         *Bitmap
}

// CompareValue compares value.
// Values should be in the range of the BSI (max, min).  If the value is outside the range, the result
// might erroneous.  The operation parameter indicates the type of comparison to be made.
// For all operations with the exception of RANGE, the value to be compared is specified by valueOrStart.
// For the RANGE parameter the comparison criteria is >= valueOrStart and <= end.
// The parallelism parameter indicates the number of CPU threads to be applied for processing.  A value
// of zero indicates that all available CPU resources will be potentially utilized.
func (b *BSI) CompareValue(parallelism int, op Operation, valueOrStart, end int64,
	foundSet *Bitmap) *Bitmap {

	comp := &task{bsi: b, op: op, valueOrStart: valueOrStart, end: end}
	if foundSet == nil {
		return parallelExecutor(parallelism, comp, compareValue, &b.eBM)
	}
	return parallelExecutor(parallelism, comp, compareValue, foundSet)
}

func compareValue(e *task, batch []uint64, resultsChan chan *Bitmap, wg *sync.WaitGroup) {

	defer wg.Done()

	results := NewBitmap()
	if e.bsi.runOptimized {
		results.RunOptimize()
	}

	x := e.bsi.BitCount()
	startIsNegative := x == 64 && uint64(e.valueOrStart)&(1<<uint64(x-1)) > 0
	endIsNegative := x == 64 && uint64(e.end)&(1<<uint64(x-1)) > 0

	for i := 0; i < len(batch); i++ {
		cID := batch[i]
		eq1, eq2 := true, true
		lt1, lt2, gt1 := false, false, false
		j := e.bsi.BitCount() - 1
		isNegative := false
		if x == 64 {
			isNegative = e.bsi.bA[j].Contains(cID)
			j--
		}
		compStartValue := e.valueOrStart
		compEndValue := e.end
		if isNegative != startIsNegative {
			compStartValue = ^e.valueOrStart + 1
		}
		if isNegative != endIsNegative {
			compEndValue = ^e.end + 1
		}
		for ; j >= 0; j-- {
			sliceContainsBit := e.bsi.bA[j].Contains(cID)

			if uint64(compStartValue)&(1<<uint64(j)) > 0 {
				// BIT in value is SET
				if !sliceContainsBit {
					if eq1 {
						if (e.op == GT || e.op == GE || e.op == RANGE) && startIsNegative && !isNegative {
							gt1 = true
						}
						if e.op == LT || e.op == LE {
							if !startIsNegative || (startIsNegative == isNegative) {
								lt1 = true
							}
						}
						eq1 = false
						break
					}
				}
			} else {
				// BIT in value is CLEAR
				if sliceContainsBit {
					if eq1 {
						if (e.op == LT || e.op == LE) && isNegative && !startIsNegative {
							lt1 = true
						}
						if e.op == GT || e.op == GE || e.op == RANGE {
							if startIsNegative || (startIsNegative == isNegative) {
								gt1 = true
							}
						}
						eq1 = false
						if e.op != RANGE {
							break
						}
					}
				}
			}

			if e.op == RANGE && uint64(compEndValue)&(1<<uint64(j)) > 0 {
				// BIT in value is SET
				if !sliceContainsBit {
					if eq2 {
						if !endIsNegative || (endIsNegative == isNegative) {
							lt2 = true
						}
						eq2 = false
						if startIsNegative && !endIsNegative {
							break
						}
					}
				}
			} else if e.op == RANGE {
				// BIT in value is CLEAR
				if sliceContainsBit {
					if eq2 {
						if isNegative && !endIsNegative {
							lt2 = true
						}
						eq2 = false
						break
					}
				}
			}

		}

		switch e.op {
		case LT:
			if lt1 {
				results.Add(cID)
			}
		case LE:
			if lt1 || (eq1 && (!startIsNegative || (startIsNegative && isNegative))) {
				results.Add(cID)
			}
		case EQ:
			if eq1 {
				results.Add(cID)
			}
		case GE:
			if gt1 || (eq1 && (startIsNegative || (!startIsNegative && !isNegative))) {
				results.Add(cID)
			}
		case GT:
			if gt1 {
				results.Add(cID)
			}
		case RANGE:
			if (eq1 || gt1) && (eq2 || lt2) {
				results.Add(cID)
			}
		default:
			panic(fmt.Sprintf("Operation [%v] not supported here", e.op))
		}
	}

	resultsChan <- results
}

// MinMax - Find minimum or maximum value.
func (b *BSI) MinMax(parallelism int, op Operation, foundSet *Bitmap) int64 {

	var n int = parallelism
	if n == 0 {
		n = runtime.NumCPU()
	}

	resultsChan := make(chan int64, n)

	card := foundSet.GetCardinality()
	x := card / uint64(n)

	remainder := card - (x * uint64(n))
	var batch []uint64
	var wg sync.WaitGroup
	iter := foundSet.ManyIterator()
	for i := 0; i < n; i++ {
		if i == n-1 {
			batch = make([]uint64, x+remainder)
		} else {
			batch = make([]uint64, x)
		}
		iter.NextMany(batch)
		wg.Add(1)
		go b.minOrMax(op, batch, resultsChan, &wg)
	}

	wg.Wait()

	close(resultsChan)
	var minMax int64
	if op == MAX {
		minMax = Min64BitSigned
	} else {
		minMax = Max64BitSigned
	}

	for val := range resultsChan {
		if (op == MAX && val > minMax) || (op == MIN && val <= minMax) {
			minMax = val
		}
	}
	return minMax
}

func (b *BSI) minOrMax(op Operation, batch []uint64, resultsChan chan int64, wg *sync.WaitGroup) {

	defer wg.Done()

	x := b.BitCount()
	var value int64 = Max64BitSigned
	if op == MAX {
		value = Min64BitSigned
	}

	for i := 0; i < len(batch); i++ {
		cID := batch[i]
		eq := true
		lt, gt := false, false
		j := b.BitCount() - 1
		var cVal int64
		valueIsNegative := uint64(value)&(1<<uint64(x-1)) > 0 && bits.Len64(uint64(value)) == 64
		isNegative := false
		if x == 64 {
			isNegative = b.bA[j].Contains(cID)
			if isNegative {
				cVal |= 1 << uint64(j)
			}
			j--
		}
		compValue := value
		if isNegative != valueIsNegative {
			compValue = ^value + 1
		}
		for ; j >= 0; j-- {
			sliceContainsBit := b.bA[j].Contains(cID)
			if sliceContainsBit {
				cVal |= 1 << uint64(j)
			}
			if uint64(compValue)&(1<<uint64(j)) > 0 {
				// BIT in value is SET
				if !sliceContainsBit {
					if eq {
						eq = false
						if op == MAX && valueIsNegative && !isNegative {
							gt = true
							break
						}
						if op == MIN && (!valueIsNegative || (valueIsNegative == isNegative)) {
							lt = true
						}
					}
				}
			} else {
				// BIT in value is CLEAR
				if sliceContainsBit {
					if eq {
						eq = false
						if op == MIN && isNegative && !valueIsNegative {
							lt = true
						}
						if op == MAX && (valueIsNegative || (valueIsNegative == isNegative)) {
							gt = true
						}
					}
				}
			}
		}
		if lt || gt {
			value = cVal
		}
	}

	resultsChan <- value
}

// Sum all values contained within the foundSet.   As a convenience, the cardinality of the foundSet
// is also returned (for calculating the average).
func (b *BSI) Sum(foundSet *Bitmap) (sum int64, count uint64) {

	count = foundSet.GetCardinality()
	var wg sync.WaitGroup
	for i := 0; i < b.BitCount(); i++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			atomic.AddInt64(&sum, int64(foundSet.AndCardinality(&b.bA[j])<<uint(j)))
		}(i)
	}
	wg.Wait()
	return
}

// Transpose calls b.IntersectAndTranspose(0, b.eBM)
func (b *BSI) Transpose() *Bitmap {
	return b.IntersectAndTranspose(0, &b.eBM)
}

// IntersectAndTranspose is a matrix transpose function.  Return a bitmap such that the values are represented as column IDs
// in the returned bitmap. This is accomplished by iterating over the foundSet and only including
// the column IDs in the source (foundSet) as compared with this BSI.  This can be useful for
// vectoring one set of integers to another.
//
// TODO: This implementation is functional but not performant, needs to be re-written perhaps using SIMD SSE2 instructions.
func (b *BSI) IntersectAndTranspose(parallelism int, foundSet *Bitmap) *Bitmap {

	trans := &task{bsi: b}
	return parallelExecutor(parallelism, trans, transpose, foundSet)
}

func transpose(e *task, batch []uint64, resultsChan chan *Bitmap, wg *sync.WaitGroup) {

	defer wg.Done()

	results := NewBitmap()
	if e.bsi.runOptimized {
		results.RunOptimize()
	}
	for _, cID := range batch {
		if value, ok := e.bsi.GetValue(uint64(cID)); ok {
			results.Add(uint64(value))
		}
	}
	resultsChan <- results
}

// ParOr is intended primarily to be a concatenation function to be used during bulk load operations.
// Care should be taken to make sure that columnIDs do not overlap (unless overlapping values are
// identical).
func (b *BSI) ParOr(parallelism int, bsis ...*BSI) {

	// Consolidate sets
	bits := len(b.bA)
	for i := 0; i < len(bsis); i++ {
		if len(bsis[i].bA) > bits {
			bits = bsis[i].BitCount()
		}
	}

	// Make sure we have enough bit slices
	for bits > b.BitCount() {
		bm := Bitmap{}
		bm.RunOptimize()
		b.bA = append(b.bA, bm)
	}

	a := make([][]*Bitmap, bits)
	for i := range a {
		a[i] = make([]*Bitmap, 0)
		for _, x := range bsis {
			if len(x.bA) > i {
				a[i] = append(a[i], &x.bA[i])
			} else {
				if b.runOptimized {
					a[i][0].RunOptimize()
				}
			}
		}
	}

	// Consolidate existence bit maps
	ebms := make([]*Bitmap, len(bsis))
	for i := range ebms {
		ebms[i] = &bsis[i].eBM
	}

	// First merge all the bit slices from all bsi maps that exist in target
	var wg sync.WaitGroup
	for i := 0; i < bits; i++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			x := []*Bitmap{&b.bA[j]}
			x = append(x, a[j]...)
			b.bA[j] = *ParOr(parallelism, x...)
		}(i)
	}
	wg.Wait()

	// merge all the EBM maps
	x := []*Bitmap{&b.eBM}
	x = append(x, ebms...)
	b.eBM = *ParOr(parallelism, x...)
}

func (b *BSI) FromBitmaps(bms []Bitmap) {
	b.eBM = bms[0]
	b.bA = bms[1:]
}

// UnmarshalBinary de-serialize a BSI.  The value at bitData[0] is the EBM.  Other indices are in least to most
// significance order starting at bitData[1] (bit position 0).
func (b *BSI) UnmarshalBinary(bitData [][]byte) error {

	for i := 1; i < len(bitData); i++ {
		if bitData == nil || len(bitData[i]) == 0 {
			continue
		}
		if b.BitCount() < i {
			newBm := Bitmap{}
			if b.runOptimized {
				newBm.RunOptimize()
			}
			b.bA = append(b.bA, newBm)
		}
		if err := b.bA[i-1].UnmarshalBinary(bitData[i]); err != nil {
			return err
		}
		if b.runOptimized {
			b.bA[i-1].RunOptimize()
		}

	}
	// First element of bitData is the EBM
	if bitData[0] == nil {
		b.eBM = Bitmap{}
		if b.runOptimized {
			b.eBM.RunOptimize()
		}
		return nil
	}
	if err := b.eBM.UnmarshalBinary(bitData[0]); err != nil {
		return err
	}
	if b.runOptimized {
		b.eBM.RunOptimize()
	}
	return nil
}

// ReadFrom reads a serialized version of this BSI from stream.
func (b *BSI) ReadFrom(stream io.Reader) (p int64, err error) {
	bm, n, err := readBSIContainerFromStream(stream)
	p += n
	if err != nil {
		err = fmt.Errorf("reading existence bitmap: %w", err)
		return
	}
	b.eBM = bm
	b.bA = b.bA[:0]
	for {
		// This forces a new memory location to be allocated and if we're lucky it only escapes if
		// there's no error.
		var bm Bitmap
		bm, n, err = readBSIContainerFromStream(stream)
		p += n
		if err == io.EOF {
			err = nil
			return
		}
		if err != nil {
			err = fmt.Errorf("reading bit slice index %v: %w", len(b.bA), err)
			return
		}
		b.bA = append(b.bA, bm)
	}
}

func readBSIContainerFromStream(r io.Reader) (bm Bitmap, p int64, err error) {
	p, err = bm.ReadFrom(r)
	return
}

// MarshalBinary serializes a BSI
func (b *BSI) MarshalBinary() ([][]byte, error) {

	var err error
	data := make([][]byte, b.BitCount()+1)
	// Add extra element for EBM (BitCount() + 1)
	for i := 1; i < b.BitCount()+1; i++ {
		data[i], err = b.bA[i-1].MarshalBinary()
		if err != nil {
			return nil, err
		}
	}
	// Marshal EBM
	data[0], err = b.eBM.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return data, nil
}

// WriteTo writes a serialized version of this BSI to stream.
func (b *BSI) WriteTo(w io.Writer) (n int64, err error) {
	n1, err := b.eBM.WriteTo(w)
	n += n1
	if err != nil {
		return
	}
	for _, bm := range b.bA {
		n1, err = bm.WriteTo(w)
		n += n1
		if err != nil {
			return
		}
	}
	return
}

// BatchEqual returns a bitmap containing the column IDs where the values are contained within the list of values provided.
func (b *BSI) BatchEqual(parallelism int, values []int64) *Bitmap {

	valMap := make(map[int64]struct{}, len(values))
	for i := 0; i < len(values); i++ {
		valMap[values[i]] = struct{}{}
	}
	comp := &task{bsi: b, values: valMap}
	return parallelExecutor(parallelism, comp, batchEqual, &b.eBM)
}

func batchEqual(e *task, batch []uint64, resultsChan chan *Bitmap,
	wg *sync.WaitGroup) {

	defer wg.Done()

	results := NewBitmap()
	if e.bsi.runOptimized {
		results.RunOptimize()
	}

	for i := 0; i < len(batch); i++ {
		cID := batch[i]
		if value, ok := e.bsi.GetValue(uint64(cID)); ok {
			if _, yes := e.values[int64(value)]; yes {
				results.Add(cID)
			}
		}
	}
	resultsChan <- results
}

// ClearValues removes the values found in foundSet
func (b *BSI) ClearValues(foundSet *Bitmap) {
	b.eBM.AndNot(foundSet)
	for i := range b.bA {
		b.bA[i].AndNot(foundSet)
	}
}

// Retains only values found in retain. Returns how many values were not retained.
func (b *BSI) Retain(retain *Bitmap) (dropped uint64) {
	preCard := b.eBM.GetCardinality()
	b.eBM.And(retain)
	dropped = preCard - b.eBM.GetCardinality()
	if dropped == 0 {
		return
	}
	for i := range b.bA {
		b.bA[i].And(retain)
	}
	return
}

// NewBSIRetainSet - Construct a new BSI from a clone of existing BSI, retain only values contained in foundSet
func (b *BSI) NewBSIRetainSet(foundSet *Bitmap) *BSI {

	newBSI := NewBSI(b.MaxValue, b.MinValue)
	newBSI.bA = make([]Bitmap, b.BitCount())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		newBSI.eBM = *b.eBM.Clone()
		newBSI.eBM.And(foundSet)
	}()
	for i := 0; i < b.BitCount(); i++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			newBSI.bA[j] = *b.bA[j].Clone()
			newBSI.bA[j].And(foundSet)
		}(i)
	}
	wg.Wait()
	return newBSI
}

// Clone performs a deep copy of BSI contents.
func (b *BSI) Clone() *BSI {
	return b.NewBSIRetainSet(&b.eBM)
}

// Add - In-place sum the contents of another BSI with this BSI, column wise.
func (b *BSI) Add(other *BSI) {

	b.eBM.Or(&other.eBM)
	for i := 0; i < len(other.bA); i++ {
		b.addDigit(&other.bA[i], i)
	}
}

func (b *BSI) addDigit(foundSet *Bitmap, i int) {

	if i >= len(b.bA) {
		b.bA = append(b.bA, Bitmap{})
	}
	carry := And(&b.bA[i], foundSet)
	b.bA[i].Xor(foundSet)
	if !carry.IsEmpty() {
		if i+1 >= len(b.bA) {
			b.bA = append(b.bA, Bitmap{})
		}
		b.addDigit(carry, i+1)
	}
}

// TransposeWithCounts is a matrix transpose function that returns a BSI that has a columnID system defined by the values
// contained within the input BSI.   Given that for BSIs, different columnIDs can have the same value.  TransposeWithCounts
// is useful for situations where there is a one-to-many relationship between the vectored integer sets.  The resulting BSI
// contains the number of times a particular value appeared in the input BSI.
func (b *BSI) TransposeWithCounts(parallelism int, foundSet, filterSet *Bitmap) *BSI {

	return parallelExecutorBSIResults(parallelism, b, transposeWithCounts, foundSet, filterSet, true)
}

func transposeWithCounts(input *BSI, filterSet *Bitmap, batch []uint64, resultsChan chan *BSI, wg *sync.WaitGroup) {

	defer wg.Done()

	results := NewDefaultBSI()
	if input.runOptimized {
		results.RunOptimize()
	}
	for _, cID := range batch {
		if value, ok := input.GetValue(uint64(cID)); ok {
			if !filterSet.Contains(uint64(value)) {
				continue
			}
			if val, ok2 := results.GetValue(uint64(value)); !ok2 {
				results.SetValue(uint64(value), 1)
			} else {
				val++
				results.SetValue(uint64(value), val)
			}
		}
	}
	resultsChan <- results
}

// Increment - In-place increment of values in a BSI.  Found set select columns for incrementing.
func (b *BSI) Increment(foundSet *Bitmap) {
	b.addDigit(foundSet, 0)
	b.eBM.Or(foundSet)
}

// IncrementAll - In-place increment of all values in a BSI.
func (b *BSI) IncrementAll() {
	b.Increment(b.GetExistenceBitmap())
}

// Equals - Check for semantic equality of two BSIs.
func (b *BSI) Equals(other *BSI) bool {
	if !b.eBM.Equals(&other.eBM) {
		return false
	}
	for i := 0; i < len(b.bA) || i < len(other.bA); i++ {
		if i >= len(b.bA) {
			if !other.bA[i].IsEmpty() {
				return false
			}
		} else if i >= len(other.bA) {
			if !b.bA[i].IsEmpty() {
				return false
			}
		} else {
			if !b.bA[i].Equals(&other.bA[i]) {
				return false
			}
		}
	}
	return true
}

// GetSizeInBytes - the size in bytes of the data structure
func (b *BSI) GetSizeInBytes() int {
	size := b.eBM.GetSizeInBytes()
	for _, bm := range b.bA {
		size += bm.GetSizeInBytes()
	}
	return int(size)
}

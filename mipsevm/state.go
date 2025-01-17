package mipsevm

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// Note: 2**12 = 4 KiB, the minimum page-size in Unicorn for mmap
	// as well as the Go runtime min phys page size.
	pageAddrSize = 12
	pageKeySize  = 32 - pageAddrSize
	pageSize     = 1 << pageAddrSize
	pageAddrMask = pageSize - 1
	maxPageCount = 1 << pageKeySize
)

type Page [pageSize]byte

func (p *Page) MarshalText() ([]byte, error) {
	dst := make([]byte, hex.EncodedLen(len(p)))
	hex.Encode(dst, p[:])
	return dst, nil
}

func (p *Page) UnmarshalText(dat []byte) error {
	if len(dat) != pageSize*2 {
		return fmt.Errorf("expected %d hex chars, but got %d", pageSize*2, len(dat))
	}
	_, err := hex.Decode(p[:], dat)
	return err
}

type State struct {
	Memory map[uint32]*Page `json:"memory"`

	PreimageKey    common.Hash `json:"preimageKey"`
	PreimageOffset uint32      `json:"preimageOffset"`

	PC     uint32 `json:"pc"`
	NextPC uint32 `json:"nextPC"`
	LO     uint32 `json:"lo"`
	HI     uint32 `json:"hi"`
	Heap   uint32 `json:"heap"` // to handle mmap growth

	ExitCode uint8 `json:"exit"`
	Exited   bool  `json:"exited"`

	Step uint64 `json:"step"`

	Registers [32]uint32 `json:"registers"`
}

func (s *State) EncodeWitness(so StateOracle) []byte {
	out := make([]byte, 0)
	memRoot := s.MerkleizeMemory(so)
	memRoot = common.Hash{31: 42} // TODO need contract to actually write memory
	out = append(out, memRoot[:]...)
	out = append(out, s.PreimageKey[:]...)
	out = binary.BigEndian.AppendUint32(out, s.PreimageOffset)
	out = binary.BigEndian.AppendUint32(out, s.PC)
	out = binary.BigEndian.AppendUint32(out, s.NextPC)
	out = binary.BigEndian.AppendUint32(out, s.LO)
	out = binary.BigEndian.AppendUint32(out, s.HI)
	out = binary.BigEndian.AppendUint32(out, s.Heap)
	out = append(out, s.ExitCode)
	if s.Exited {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	out = binary.BigEndian.AppendUint64(out, s.Step)
	for _, r := range s.Registers {
		out = binary.BigEndian.AppendUint32(out, r)
	}
	return out
}

func (s *State) MerkleizeMemory(so StateOracle) [32]byte {
	// empty parts of the tree are all zero. Precompute the hash of each full-zero range sub-tree level.
	var zeroHashes [256][32]byte
	for i := 1; i < 256; i++ {
		zeroHashes[i] = so.Remember(zeroHashes[i-1], zeroHashes[i-1])
	}
	// for each page, remember the generalized indices leading up to that page in the memory tree,
	// so we can deduplicate work.
	pageBranches := make(map[uint64]struct{})
	for pageKey := range s.Memory {
		pageGindex := (1 << pageKeySize) | uint64(pageKey)
		for i := 0; i < pageKeySize; i++ {
			gindex := pageGindex >> i
			pageBranches[gindex] = struct{}{}
		}
	}
	// helper func to merkleize a complete power-of-2 subtree, with stack-wise operation
	merkleize := func(stackDepth uint64, getItem func(index uint64) [32]byte) [32]byte {
		stack := make([][32]byte, stackDepth+1)
		for i := uint64(0); i < (1 << stackDepth); i++ {
			v := getItem(i)
			for j := uint64(0); j <= stackDepth; j++ {
				if i&(1<<j) == 0 {
					stack[j] = v
					break
				} else {
					v = so.Remember(stack[j], v)
				}
			}
		}
		return stack[stackDepth]
	}
	merkleizePage := func(page *Page) [32]byte {
		return merkleize(pageAddrSize-5, func(index uint64) [32]byte { // 32 byte leaf values (5 bits)
			return *(*[32]byte)(page[index*32 : index*32+32])
		})
	}
	// Function to merkleize a memory sub-tree. Once it reaches the depth of a specific page, it merkleizes as page.
	var merkleizeMemory func(gindex uint64, depth uint64) [32]byte
	merkleizeMemory = func(gindex uint64, depth uint64) [32]byte {
		if depth == pageKeySize {
			pageKey := uint32(gindex & ((1 << pageKeySize) - 1))
			return merkleizePage(s.Memory[pageKey])
		}
		left := gindex << 1
		right := left | 1
		var leftRoot, rightRoot [32]byte
		if _, ok := pageBranches[left]; ok {
			leftRoot = merkleizeMemory(left, depth+1)
		} else {
			leftRoot = zeroHashes[pageKeySize-(depth+1)+(pageAddrSize-5)]
		}
		if _, ok := pageBranches[right]; ok {
			rightRoot = merkleizeMemory(right, depth+1)
		} else {
			rightRoot = zeroHashes[pageKeySize-(depth+1)+(pageAddrSize-5)]
		}
		return so.Remember(leftRoot, rightRoot)
	}
	return merkleizeMemory(1, 0)
}

func (s *State) SetMemory(addr uint32, v uint32) {
	// addr must be aligned to 4 bytes
	if addr&0x3 != 0 {
		panic(fmt.Errorf("unaligned memory access: %x", addr))
	}
	pageIndex := addr >> pageAddrSize
	pageAddr := addr & pageAddrMask
	p, ok := s.Memory[pageIndex]
	if !ok {
		// allocate the page if we have not already.
		// Go may mmap relatively large ranges, but we only allocate the pages just in time.
		p = &Page{}
		s.Memory[pageIndex] = p
	}
	binary.BigEndian.PutUint32(p[pageAddr:pageAddr+4], v)
}

func (s *State) GetMemory(addr uint32) uint32 {
	// addr must be aligned to 4 bytes
	if addr&0x3 != 0 {
		panic(fmt.Errorf("unaligned memory access: %x", addr))
	}
	p, ok := s.Memory[addr>>pageAddrSize]
	if !ok {
		return 0
	}
	pageAddr := addr & pageAddrMask
	return binary.BigEndian.Uint32(p[pageAddr : pageAddr+4])
}

func (s *State) SetMemoryRange(addr uint32, r io.Reader) error {
	for {
		pageIndex := addr >> pageAddrSize
		pageAddr := addr & pageAddrMask
		p, ok := s.Memory[pageIndex]
		if !ok {
			p = &Page{}
			s.Memory[pageIndex] = p
		}
		n, err := r.Read(p[pageAddr:])
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		addr += uint32(n)
	}
}

type memReader struct {
	state *State
	addr  uint32
	count uint32
}

func (r *memReader) Read(dest []byte) (n int, err error) {
	if r.count == 0 {
		return 0, io.EOF
	}

	// Keep iterating over memory until we have all our data.
	// It may wrap around the address range, and may not be aligned
	endAddr := r.addr + r.count

	pageIndex := r.addr >> pageAddrSize
	start := r.addr & pageAddrMask
	end := uint32(pageSize)

	if pageIndex == (endAddr >> pageAddrSize) {
		end = endAddr & pageAddrMask
	}
	p, ok := r.state.Memory[pageIndex]
	if ok {
		n = copy(dest, p[start:end])
	} else {
		n = copy(dest, make([]byte, end-start)) // default to zeroes
	}
	r.addr += uint32(n)
	r.count -= uint32(n)
	return n, nil
}

func (s *State) ReadMemoryRange(addr uint32, count uint32) io.Reader {
	return &memReader{state: s, addr: addr, count: count}
}

// TODO convert access-list to calldata and state-sets for EVM

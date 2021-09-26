//go:build mips
// +build mips

package oracle

import (
	"math/big"
	"os"
	"reflect"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var preimages = make(map[common.Hash][]byte)
var inputs [7]common.Hash
var inputsLoaded bool = false

func byteAt(addr uint64, length int) []byte {
	var ret []byte
	bh := (*reflect.SliceHeader)(unsafe.Pointer(&ret))
	bh.Data = uintptr(addr)
	bh.Len = length
	bh.Cap = length
	return ret
}

func Input(index int) common.Hash {
	if index < 0 || index > 6 {
		panic("bad input index")
	}
	if !inputsLoaded {
		// before this isn't run on chain (confirm this isn't cached)
		// does this interact with the GC?
		ret := byteAt(0x30000000, len(inputs)*0x20)

		os.Stderr.WriteString("********* on chain starts here *********\n")

		for i := 0; i < len(inputs); i++ {
			inputs[i] = common.BytesToHash(ret[i*0x20 : i*0x20+0x20])
			//fmt.Println(i, inputs[i])
		}

		inputsLoaded = true
	}
	return inputs[index]
}

func Halt() {
	os.Stderr.WriteString("THIS SHOULD BE PATCHED OUT\n")
	os.Exit(0)
}

func Output(output common.Hash) {
	ret := byteAt(0x30000800, 0x20)
	copy(ret, output.Bytes())
	Halt()
}

func Preimage(hash common.Hash) []byte {
	val, ok := preimages[hash]
	if !ok {
		// load in hash
		preImageHash := byteAt(0x30001000, 0x20)
		copy(preImageHash, hash.Bytes())

		// used in unicorn emulator to trigger the load
		// in onchain mips, it's instant
		os.Getpid()

		// ready
		rawSize := common.CopyBytes(byteAt(0x31000000, 4))
		size := (int(rawSize[0]) << 24) | (int(rawSize[1]) << 16) | (int(rawSize[2]) << 8) | int(rawSize[3])
		ret := common.CopyBytes(byteAt(0x31000004, size))

		realhash := crypto.Keccak256Hash(ret)
		if realhash != hash {
			panic("preimage has wrong hash")
		}

		preimages[hash] = ret
		return ret
	}
	return val
}

// these are stubs in embedded world
func PrefetchStorage(*big.Int, common.Address, common.Hash, func(map[common.Hash][]byte)) {}
func PrefetchAccount(*big.Int, common.Address, func(map[common.Hash][]byte))              {}
func PrefetchCode(blockNumber *big.Int, addrHash common.Hash)                             {}
func PrefetchBlock(blockNumber *big.Int, startBlock bool, hasher types.TrieHasher)        {}

// KeyValueWriter wraps the Put method of a backing data store.
type PreimageKeyValueWriter struct{}

func (kw PreimageKeyValueWriter) Put(key []byte, value []byte) error { return nil }
func (kw PreimageKeyValueWriter) Delete(key []byte) error            { return nil }
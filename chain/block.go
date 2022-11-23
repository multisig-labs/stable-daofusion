// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package chain

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	log "github.com/inconshreveable/log15"
)

const futureBound = 10 * time.Second

var _ snowman.Block = &StatelessBlock{}

type StatefulBlock struct {
	Prnt        ids.ID         `serialize:"true" json:"parent"`
	Tmstmp      int64          `serialize:"true" json:"timestamp"`
	Hght        uint64         `serialize:"true" json:"height"`
	Price       uint64         `serialize:"true" json:"price"`
	Cost        uint64         `serialize:"true" json:"cost"`
	AccessProof common.Hash    `serialize:"true" json:"accessProof"`
	Txs         []*Transaction `serialize:"true" json:"txs"`
}

// Stateless is defined separately from "Block"
// in case external packages needs use the stateful block
// without mocking VM or parent block
type StatelessBlock struct {
	*StatefulBlock `serialize:"true" json:"block"`

	id    ids.ID
	st    choices.Status
	t     time.Time
	bytes []byte

	vm         VM
	children   []*StatelessBlock
	onAcceptDB *versiondb.Database
}

func NewBlock(vm VM, parent snowman.Block, tmstp int64, context *Context) *StatelessBlock {
	return &StatelessBlock{
		StatefulBlock: &StatefulBlock{
			Tmstmp: tmstp,
			Prnt:   parent.ID(),
			Hght:   parent.Height() + 1,
			Price:  context.NextPrice,
			Cost:   context.NextCost,
		},
		vm: vm,
		st: choices.Processing,
	}
}

func ParseBlock(
	source []byte,
	status choices.Status,
	vm VM,
) (*StatelessBlock, error) {
	blk := new(StatefulBlock)
	if _, err := Unmarshal(source, blk); err != nil {
		return nil, err
	}
	return ParseStatefulBlock(blk, source, status, vm)
}

func ParseStatefulBlock(
	blk *StatefulBlock,
	source []byte,
	status choices.Status,
	vm VM,
) (*StatelessBlock, error) {
	if len(source) == 0 {
		b, err := Marshal(blk)
		if err != nil {
			return nil, err
		}
		source = b
	}
	b := &StatelessBlock{
		StatefulBlock: blk,
		t:             time.Unix(blk.Tmstmp, 0),
		bytes:         source,
		st:            status,
		vm:            vm,
	}
	id, err := ids.ToID(crypto.Keccak256(b.bytes))
	if err != nil {
		return nil, err
	}
	b.id = id
	g := vm.Genesis()
	for _, tx := range blk.Txs {
		if err := tx.Init(g); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *StatelessBlock) init() error {
	bytes, err := Marshal(b.StatefulBlock)
	if err != nil {
		return err
	}
	b.bytes = bytes

	id, err := ids.ToID(crypto.Keccak256(b.bytes))
	if err != nil {
		return err
	}
	b.id = id
	b.t = time.Unix(b.StatefulBlock.Tmstmp, 0)
	g := b.vm.Genesis()
	for _, tx := range b.StatefulBlock.Txs {
		if err := tx.Init(g); err != nil {
			return err
		}
	}
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) ID() ids.ID { return b.id }

func generateAccessProof(db database.Database, pid ids.ID, hght uint64) common.Hash {
	// This seed selection is gameable because the previous block producer could
	// TODO what does this mean? why does it matter? negative consequence of this?
	// grind the block hash to bias value selection (by including different
	// transactions). This could be improved with some form of a VRF.
	seed := make([]byte, 40) // hash [32] + uint64 [8]
	copy(seed, pid[:])       // copy hash to make sure not overwritten
	binary.LittleEndian.PutUint64(seed[32:], hght)

	v := SelectRandomValue(db, seed)
	if len(v) == 0 {
		log.Debug("no key found for access proof", "parent", hexutil.Encode(pid[:]), "height", hght)
		return common.Hash{}
	}

	rand := ValueHash(append(v, pid[:]...))
	log.Debug("generated access proof", "random", rand, "parent", hexutil.Encode(pid[:]), "key", v)
	return rand
}

// verify checks the correctness of a block and then returns the
// *versiondb.Database computed during execution.
func (b *StatelessBlock) verify() (*StatelessBlock, *versiondb.Database, error) {
	g := b.vm.Genesis()

	// Perform basic correctness checks before doing any expensive work
	if len(b.Txs) == 0 {
		return nil, nil, ErrNoTxs
	}
	if b.Timestamp().Unix() >= time.Now().Add(futureBound).Unix() {
		return nil, nil, ErrTimestampTooLate
	}
	blockSize := uint64(0)
	for _, tx := range b.Txs {
		blockSize += tx.LoadUnits(g)
		if blockSize > g.MaxBlockSize {
			return nil, nil, ErrBlockTooBig
		}
	}

	// Verify parent is available
	parent, err := b.vm.GetStatelessBlock(b.Prnt)
	if err != nil {
		log.Debug("could not get parent", "id", b.Prnt)
		return nil, nil, err
	}
	if b.Timestamp().Unix() < parent.Timestamp().Unix() {
		return nil, nil, ErrTimestampTooEarly
	}

	context, err := b.vm.ExecutionContext(b.Tmstmp, parent)
	if err != nil {
		return nil, nil, err
	}
	if b.Cost != context.NextCost {
		return nil, nil, ErrInvalidCost
	}
	if b.Price != context.NextPrice {
		return nil, nil, ErrInvalidPrice
	}

	parentState, err := parent.onAccept()
	if err != nil {
		return nil, nil, err
	}
	onAcceptDB := versiondb.New(parentState)

	// Generate access proof from random value
	accessProof := generateAccessProof(onAcceptDB, parent.ID(), b.Hght)
	if b.AccessProof != accessProof {
		return nil, nil, ErrInvalidAccessProof
	}

	// Process new transactions
	log.Debug("build context", "height", b.Hght, "price", b.Price, "cost", b.Cost)
	surplusFee := uint64(0)
	for _, tx := range b.Txs {
		if err := tx.Execute(g, onAcceptDB, b, context); err != nil {
			return nil, nil, err
		}
		surplusFee += (tx.GetPrice() - b.Price) * tx.FeeUnits(g)
	}
	// Ensure enough fee is paid to compensate for block production speed
	requiredSurplus := b.Price * b.Cost
	if surplusFee < requiredSurplus {
		return nil, nil, fmt.Errorf("%w: required=%d found=%d", ErrInsufficientSurplus, requiredSurplus, surplusFee)
	}
	return parent, onAcceptDB, nil
}

// implements "snowman.Block"
func (b *StatelessBlock) Verify() error {
	parent, onAcceptDB, err := b.verify()
	if err != nil {
		log.Debug("block verification failed", "blkID", b.ID(), "error", err)
		return err
	}
	b.onAcceptDB = onAcceptDB

	// Set last accepted block and store
	if err := SetLastAccepted(b.onAcceptDB, b); err != nil {
		return err
	}

	parent.addChild(b)
	b.vm.Verified(b)
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) Accept() error {
	if err := b.onAcceptDB.Commit(); err != nil {
		return err
	}
	for _, child := range b.children {
		if err := child.onAcceptDB.SetDatabase(b.vm.State()); err != nil {
			return err
		}
	}
	b.st = choices.Accepted
	b.vm.Accepted(b)
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) Reject() error {
	b.st = choices.Rejected
	b.vm.Rejected(b)
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) Status() choices.Status { return b.st }

// implements "snowman.Block"
func (b *StatelessBlock) Parent() ids.ID { return b.StatefulBlock.Prnt }

// implements "snowman.Block"
func (b *StatelessBlock) Bytes() []byte { return b.bytes }

// implements "snowman.Block"
func (b *StatelessBlock) Height() uint64 { return b.StatefulBlock.Hght }

// implements "snowman.Block"
func (b *StatelessBlock) Timestamp() time.Time { return b.t }

func (b *StatelessBlock) SetChildrenDB(db database.Database) error {
	for _, child := range b.children {
		if err := child.onAcceptDB.SetDatabase(db); err != nil {
			return err
		}
	}
	return nil
}

func (b *StatelessBlock) onAccept() (database.Database, error) {
	if b.st == choices.Accepted || b.Hght == 0 /* genesis */ {
		return b.vm.State(), nil
	}
	if b.onAcceptDB != nil {
		return b.onAcceptDB, nil
	}
	return nil, ErrParentBlockNotVerified
}

func (b *StatelessBlock) addChild(c *StatelessBlock) {
	b.children = append(b.children, c)
}

// DummyBlock is used for validating new txs and some tests
func DummyBlock(tmstp int64, tx *Transaction) *StatelessBlock {
	return &StatelessBlock{
		StatefulBlock: &StatefulBlock{
			Tmstmp: tmstp, Txs: []*Transaction{tx},
		},
	}
}

func (b *StatefulBlock) Dummy() bool { return b.Prnt == ids.Empty }

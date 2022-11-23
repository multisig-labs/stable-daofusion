// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package vm

import (
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/blobvm/utils/timer"
	log "github.com/inconshreveable/log15"
)

type BlockBuilder interface {
	Build()
	Gossip()
	HandleGenerateBlock()
}

var (
	_ BlockBuilder = (*TimeBuilder)(nil)
	_ BlockBuilder = (*ManualBuilder)(nil)
)

// [SetBlockBuilder] changes the [BlockBuilder] during runtime by stopping the
// previous block builder and then starting a new one.
// TODO who typically calls this?
func (vm *VM) SetBlockBuilder(b func() BlockBuilder) {
	// Wait for previous builder to stop
	close(vm.builderStop)
	<-vm.doneBuild
	<-vm.doneGossip

	// Reset channels to make sure newly assigned builder shuts down correctly
	vm.doneBuild = make(chan struct{})
	vm.doneGossip = make(chan struct{})
	vm.builderStop = make(chan struct{})

	// Start new builder
	vm.builder = b()
	go vm.builder.Build()
	go vm.builder.Gossip()
}

// buildingBlkStatus denotes the current status of the VM in block production.
type buildingBlkStatus uint8

const (
	dontBuild buildingBlkStatus = iota
	mayBuild
	building
)

// TimeBuilder tells the engine when to build blocks and gossip transactions
type TimeBuilder struct {
	vm *VM

	// [l] must be held when accessing [buildStatus]
	l sync.Mutex

	// status signals the phase of block building the VM is currently in.
	// [dontBuild] indicates there's no need to build a block.
	// [mayBuild] indicates the VM should proceed to build a block.
	// [building] indicates the VM has sent a request to the engine to build a block.
	// TODO its signaling to avalanchego code, right? does this get propogated? what happens with the signal in avalanchego?
	// TODO [mainq] once it's signalled, how does a proposer get selected and hows it start the block building process in the vm? wheres the vm code?
	// the box's consensus engine picks the box's vm to be the proposer.
	// vm code starts with vm.go -> BuildBlock , which calls into builder.go -> BuildBlock
	// the vm code is builder.go -> BuildBlock
	status buildingBlkStatus

	// [buildBlockTimer] is a two stage timer handling block production.
	// Stage1 build a block if the batch size has been reached.
	// Stage2 build a block regardless of the size.
	buildBlockTimer *timer.Timer

	stop        chan struct{}
	builderStop chan struct{}

	doneBuild  chan struct{}
	doneGossip chan struct{}
}

func (vm *VM) NewTimeBuilder() *TimeBuilder {
	b := &TimeBuilder{
		vm:          vm,
		status:      dontBuild,
		builderStop: vm.builderStop,
		stop:        vm.stop,
		doneBuild:   vm.doneBuild,
		doneGossip:  vm.doneGossip,
	}
	b.buildBlockTimer = timer.NewStagedTimer(b.buildBlockTwoStageTimer)
	return b
}

// signalTxsReady sets the initial timeout on the two stage timer if the process
// has not already begun from an earlier notification. If [buildStatus] is anything
// other than [dontBuild], then the attempt has already begun and this notification
// can be safely skipped.
func (b *TimeBuilder) signalTxsReady() {
	b.l.Lock()
	defer b.l.Unlock()

	if b.status != dontBuild {
		return
	}

	b.markBuilding()
}

// signal the avalanchego engine
// to build a block from pending transactions
// TODO what happens after it signals the avalanchego engine? whats the engine do and wheres that code?
// TODO how does a node get selected to check the mempool and signal to the network a new block can get built?
func (b *TimeBuilder) markBuilding() {
	select {
	case b.vm.toEngine <- common.PendingTxs:
		b.status = building
	default:
		log.Debug("dropping message to consensus engine")
	}
}

// HandleGenerateBlock should be called immediately after [BuildBlock].
// [HandleGenerateBlock] invocation could lead to quiesence, building a block with
// some delay, or attempting to build another block immediately.
func (b *TimeBuilder) HandleGenerateBlock() {
	b.l.Lock()
	defer b.l.Unlock()

	// If we still need to build a block immediately after building, we let the
	// engine know it [mayBuild] in [buildInterval].
	if b.needToBuild() {
		b.status = mayBuild
		b.buildBlockTimer.SetTimeoutIn(b.vm.config.BuildInterval)
	} else {
		b.status = dontBuild
	}
}

// needToBuild returns true if there are outstanding transactions to be issued
// into a block.
func (b *TimeBuilder) needToBuild() bool {
	return b.vm.mempool.Len() > 0
}

// buildBlockTwoStageTimer is a two stage timer that sends a notification
// to the engine when the VM is ready to build a block.
// If it should be called back again, it returns the timeout duration at
// which it should be called again.
func (b *TimeBuilder) buildBlockTwoStageTimer() (time.Duration, bool) {
	b.l.Lock()
	defer b.l.Unlock()

	switch b.status {
	case dontBuild:
	case mayBuild:
		b.markBuilding()
	case building:
		// If the status has already been set to building, there is no need
		// to send an additional request to the consensus engine until the call
		// to BuildBlock resets the block status.
	default:
		// Log an error if an invalid status is found.
		log.Error("Found invalid build status in build block timer", "status", b.status)
	}

	// No need for the timeout to fire again until BuildBlock is called.
	return 0, false
}

func (b *TimeBuilder) Build() {
	log.Debug("starting build loops")
	defer close(b.doneBuild)

	for {
		select {
		case <-b.vm.mempool.Pending:
			b.signalTxsReady()
		case <-b.builderStop:
			return
		case <-b.stop:
			return
		}
	}
}

// periodically but less aggressively force-regossip the pending
func (b *TimeBuilder) Gossip() {
	log.Debug("starting gossip loops")
	defer close(b.doneGossip)

	g := time.NewTicker(b.vm.config.GossipInterval)
	defer g.Stop()

	rg := time.NewTicker(b.vm.config.RegossipInterval)
	defer rg.Stop()

	for {
		select {
		case <-g.C:
			newTxs := b.vm.mempool.NewTxs(b.vm.genesis.TargetBlockSize)
			_ = b.vm.network.GossipNewTxs(newTxs) // handles case where there are none
		case <-rg.C:
			_ = b.vm.network.RegossipTxs()
		case <-b.builderStop:
			return
		case <-b.stop:
			return
		}
	}
}

type ManualBuilder struct {
	vm         *VM
	doneBuild  chan struct{}
	doneGossip chan struct{}
}

func (vm *VM) NewManualBuilder() *ManualBuilder {
	return &ManualBuilder{
		vm:         vm,
		doneBuild:  vm.doneBuild,
		doneGossip: vm.doneGossip,
	}
}

func (b *ManualBuilder) Build() {
	close(b.doneBuild)
}

func (b *ManualBuilder) Gossip() {
	close(b.doneGossip)
}
func (b *ManualBuilder) HandleGenerateBlock() {}
func (b *ManualBuilder) NotifyBuild() {
	select {
	case b.vm.toEngine <- common.PendingTxs:
	default:
		log.Debug("dropping message to consensus engine")
	}
}

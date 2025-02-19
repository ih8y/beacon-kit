// SPDX-License-Identifier: BUSL-1.1
//
// Copyright (C) 2024, Berachain Foundation. All rights reserved.
// Use of this software is governed by the Business Source License included
// in the LICENSE file of this repository and at www.mariadb.com/bsl11.
//
// ANY USE OF THE LICENSED WORK IN VIOLATION OF THIS LICENSE WILL AUTOMATICALLY
// TERMINATE YOUR RIGHTS UNDER THIS LICENSE FOR THE CURRENT AND ALL OTHER
// VERSIONS OF THE LICENSED WORK.
//
// THIS LICENSE DOES NOT GRANT YOU ANY RIGHT IN ANY TRADEMARK OR LOGO OF
// LICENSOR OR ITS AFFILIATES (PROVIDED THAT YOU MAY USE A TRADEMARK OR LOGO OF
// LICENSOR AS EXPRESSLY REQUIRED BY THIS LICENSE).
//
// TO THE EXTENT PERMITTED BY APPLICABLE LAW, THE LICENSED WORK IS PROVIDED ON
// AN “AS IS” BASIS. LICENSOR HEREBY DISCLAIMS ALL WARRANTIES AND CONDITIONS,
// EXPRESS OR IMPLIED, INCLUDING (WITHOUT LIMITATION) WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, AND
// TITLE.
//
//nolint:contextcheck // its fine.
package cometbft

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"cosmossdk.io/store/rootmulti"
	ctypes "github.com/berachain/beacon-kit/mod/consensus-types/pkg/types"
	servercmtlog "github.com/berachain/beacon-kit/mod/consensus/pkg/cometbft/service/log"
	"github.com/berachain/beacon-kit/mod/consensus/pkg/types"
	errorsmod "github.com/berachain/beacon-kit/mod/errors"
	"github.com/berachain/beacon-kit/mod/primitives/pkg/encoding/json"
	math "github.com/berachain/beacon-kit/mod/primitives/pkg/math"
	cmtabci "github.com/cometbft/cometbft/abci/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	sdkversion "github.com/cosmos/cosmos-sdk/version"
	"github.com/sourcegraph/conc/iter"
)

var (
	errInvalidHeight         = errors.New("invalid height")
	errNilFinalizeBlockState = errors.New("finalizeBlockState is nil")
)

func (s *Service[LoggerT]) InitChain(
	_ context.Context,
	req *cmtabci.InitChainRequest,
) (*cmtabci.InitChainResponse, error) {
	if req.ChainId != s.chainID {
		return nil, fmt.Errorf(
			"invalid chain-id on InitChain; expected: %s, got: %s",
			s.chainID,
			req.ChainId,
		)
	}

	s.logger.Info(
		"InitChain",
		"initialHeight",
		req.InitialHeight,
		"chainID",
		req.ChainId,
	)

	// Set the initial height, which will be used to determine if we are
	// proposing
	// or processing the first block or not.
	s.initialHeight = req.InitialHeight
	if s.initialHeight == 0 {
		s.initialHeight = 1
	}

	// if req.InitialHeight is > 1, then we set the initial version on all
	// stores
	if req.InitialHeight > 1 {
		if err := s.sm.CommitMultiStore().
			SetInitialVersion(req.InitialHeight); err != nil {
			return nil, err
		}
	}

	s.finalizeBlockState = s.resetState()

	resValidators, err := s.initChainer(
		s.finalizeBlockState.Context(),
		req.AppStateBytes,
	)
	if err != nil {
		return nil, err
	}

	// check validators
	if len(req.Validators) > 0 {
		if len(req.Validators) != len(resValidators) {
			return nil, fmt.Errorf(
				"len(RequestInitChain.Validators) != len(GenesisValidators) (%d != %d)",
				len(req.Validators),
				len(resValidators),
			)
		}

		sort.Sort(cmtabci.ValidatorUpdates(req.Validators))

		for i := range resValidators {
			if req.Validators[i].Power != resValidators[i].Power {
				return nil, errors.New("mismatched power")
			}
			if !bytes.Equal(
				req.Validators[i].PubKeyBytes, resValidators[i].
					PubKeyBytes) {
				return nil, errors.New("mismatched pubkey bytes")
			}

			if req.Validators[i].PubKeyType !=
				resValidators[i].PubKeyType {
				return nil, errors.New("mismatched pubkey types")
			}
		}
	}

	// NOTE: We don't commit, but FinalizeBlock for block InitialHeight starts
	// from
	// this FinalizeBlockState.
	return &cmtabci.InitChainResponse{
		ConsensusParams: req.ConsensusParams,
		Validators:      resValidators,
		AppHash:         s.sm.CommitMultiStore().LastCommitID().Hash,
	}, nil
}

// InitChainer initializes the chain.
func (s *Service[LoggerT]) initChainer(
	ctx sdk.Context,
	appStateBytes []byte,
) ([]cmtabci.ValidatorUpdate, error) {
	var genesisState map[string]json.RawMessage
	if err := json.Unmarshal(appStateBytes, &genesisState); err != nil {
		return nil, err
	}
	valUpdates, err := s.Middleware.InitGenesis(
		ctx,
		[]byte(genesisState["beacon"]),
	)
	if err != nil {
		return nil, err
	}

	return iter.MapErr(
		valUpdates,
		convertValidatorUpdate[cmtabci.ValidatorUpdate],
	)
}

func (s *Service[LoggerT]) Info(
	context.Context,
	*cmtabci.InfoRequest,
) (*cmtabci.InfoResponse, error) {
	lastCommitID := s.sm.CommitMultiStore().LastCommitID()
	appVersion := initialAppVersion
	if lastCommitID.Version > 0 {
		var err error
		appVersion, err = s.appVersion()
		if err != nil {
			return nil, fmt.Errorf("failed getting app version: %w", err)
		}
	}

	return &cmtabci.InfoResponse{
		Data:             appName,
		Version:          sdkversion.Version,
		AppVersion:       appVersion,
		LastBlockHeight:  lastCommitID.Version,
		LastBlockAppHash: lastCommitID.Hash,
	}, nil
}

// PrepareProposal implements the PrepareProposal ABCI method and returns a
// ResponsePrepareProposal object to the client.
func (s *Service[LoggerT]) PrepareProposal(
	_ context.Context,
	req *cmtabci.PrepareProposalRequest,
) (*cmtabci.PrepareProposalResponse, error) {
	// CometBFT must never call PrepareProposal with a height of 0.
	if req.Height < 1 {
		return nil, fmt.Errorf(
			"prepareProposal at height %v: %w",
			req.Height,
			errInvalidHeight,
		)
	}

	// Always reset state given that PrepareProposal can timeout
	// and be called again in a subsequent round.
	s.prepareProposalState = s.resetState()
	s.prepareProposalState.SetContext(
		s.getContextForProposal(
			s.prepareProposalState.Context(),
			req.Height,
		),
	)

	blkBz, sidecarsBz, err := s.Middleware.PrepareProposal(
		s.prepareProposalState.Context(), &types.SlotData[
			*ctypes.AttestationData,
			*ctypes.SlashingInfo,
		]{
			Slot: math.Slot(req.Height),
		},
	)
	if err != nil {
		s.logger.Error(
			"failed to prepare proposal",
			"height",
			req.Height,
			"time",
			req.Time,
			"err",
			err,
		)
		return &cmtabci.PrepareProposalResponse{Txs: req.Txs}, nil
	}

	return &cmtabci.PrepareProposalResponse{
		Txs: [][]byte{blkBz, sidecarsBz},
	}, nil
}

// ProcessProposal implements the ProcessProposal ABCI method and returns a
// ResponseProcessProposal object to the client.
func (s *Service[LoggerT]) ProcessProposal(
	_ context.Context,
	req *cmtabci.ProcessProposalRequest,
) (*cmtabci.ProcessProposalResponse, error) {
	// CometBFT must never call ProcessProposal with a height of 0.
	if req.Height < 1 {
		return nil, fmt.Errorf(
			"processProposal at height %v: %w",
			req.Height,
			errInvalidHeight,
		)
	}

	// Since the application can get access to FinalizeBlock state and write to
	// it, we must be sure to reset it in case ProcessProposal timeouts and is
	// called
	// again in a subsequent round. However, we only want to do this after we've
	// processed the first block, as we want to avoid overwriting the
	// finalizeState
	// after state changes during InitChain.
	s.processProposalState = s.resetState()
	if req.Height > s.initialHeight {
		s.finalizeBlockState = s.resetState()
	}

	s.processProposalState.SetContext(
		s.getContextForProposal(
			s.processProposalState.Context(),
			req.Height,
		),
	)

	resp, err := s.Middleware.ProcessProposal(
		s.processProposalState.Context(),
		req,
	)
	if err != nil {
		s.logger.Error(
			"failed to process proposal",
			"height",
			req.Height,
			"time",
			req.Time,
			"hash",
			fmt.Sprintf("%X", req.Hash),
			"err",
			err,
		)
		return &cmtabci.ProcessProposalResponse{
			Status: cmtabci.PROCESS_PROPOSAL_STATUS_REJECT,
		}, nil
	}

	return resp, nil
}

func (s *Service[LoggerT]) internalFinalizeBlock(
	req *cmtabci.FinalizeBlockRequest,
) (*cmtabci.FinalizeBlockResponse, error) {
	if err := s.validateFinalizeBlockHeight(req); err != nil {
		return nil, err
	}

	// finalizeBlockState should be set on InitChain or ProcessProposal. If it
	// is nil, it means we are replaying this block and we need to set the state
	// here given that during block replay ProcessProposal is not executed by
	// CometBFT.
	if s.finalizeBlockState == nil {
		s.finalizeBlockState = s.resetState()
	}

	// Iterate over all raw transactions in the proposal and attempt to execute
	// them, gathering the execution results.
	//
	// NOTE: Not all raw transactions may adhere to the sdk.Tx interface, e.g.
	// vote extensions, so skip those.
	txResults := make([]*cmtabci.ExecTxResult, 0, len(req.Txs))
	for range req.Txs {
		//nolint:mnd // its okay for now.
		txResults = append(txResults, &cmtabci.ExecTxResult{
			Codespace: "sdk",
			Code:      2,
			Log:       "skip decoding",
			GasWanted: 0,
			GasUsed:   0,
		})
	}

	finalizeBlock, err := s.Middleware.FinalizeBlock(
		s.finalizeBlockState.Context(),
		req,
	)
	if err != nil {
		return nil, err
	}

	valUpdates, err := iter.MapErr(
		finalizeBlock,
		convertValidatorUpdate[cmtabci.ValidatorUpdate],
	)
	if err != nil {
		return nil, err
	}

	return &cmtabci.FinalizeBlockResponse{
		TxResults:             txResults,
		ValidatorUpdates:      valUpdates,
		ConsensusParamUpdates: s.paramStore.Get(),
	}, nil
}

func (s *Service[_]) validateFinalizeBlockHeight(
	req *cmtabci.FinalizeBlockRequest,
) error {
	if req.Height < 1 {
		return fmt.Errorf(
			"finalizeBlock at height %v: %w",
			req.Height,
			errInvalidHeight,
		)
	}

	lastBlockHeight := s.LastBlockHeight()

	// expectedHeight holds the expected height to validate
	var expectedHeight int64
	if lastBlockHeight == 0 && s.initialHeight > 1 {
		// In this case, we're validating the first block of the chain, i.e no
		// previous commit. The height we're expecting is the initial height.
		expectedHeight = s.initialHeight
	} else {
		// This case can mean two things:
		//
		// - Either there was already a previous commit in the store, in which
		// case we increment the version from there.
		// - Or there was no previous commit, in which case we start at version
		// 1.
		expectedHeight = lastBlockHeight + 1
	}

	if req.Height != expectedHeight {
		return fmt.Errorf(
			"invalid height: %d; expected: %d",
			req.Height,
			expectedHeight,
		)
	}

	return nil
}

func (s *Service[_]) FinalizeBlock(
	_ context.Context,
	req *cmtabci.FinalizeBlockRequest,
) (*cmtabci.FinalizeBlockResponse, error) {
	res, err := s.internalFinalizeBlock(req)
	if res != nil {
		res.AppHash = s.workingHash()
	}

	return res, err
}

// Commit implements the ABCI interface. It will commit all state that exists in
// the deliver state's multi-store and includes the resulting commit ID in the
// returned cmtabci.ResponseCommit. Commit will set the check state based on the
// latest header and reset the deliver state. Also, if a non-zero halt height is
// defined in config, Commit will execute a deferred function call to check
// against that height and gracefully halt if it matches the latest committed
// height.
func (s *Service[LoggerT]) Commit(
	context.Context, *cmtabci.CommitRequest,
) (*cmtabci.CommitResponse, error) {
	if s.finalizeBlockState == nil {
		// This is unexpected since CometBFT should call Commit only
		// after FinalizeBlock has been called. Panic appeases nilaway.
		panic(fmt.Errorf("commit: %w", errNilFinalizeBlockState))
	}
	header := s.finalizeBlockState.Context().BlockHeader()
	retainHeight := s.GetBlockRetentionHeight(header.Height)

	rms, ok := s.sm.CommitMultiStore().(*rootmulti.Store)
	if ok {
		rms.SetCommitHeader(header)
	}
	s.sm.CommitMultiStore().Commit()

	s.finalizeBlockState = nil

	return &cmtabci.CommitResponse{
		RetainHeight: retainHeight,
	}, nil
}

// workingHash gets the apphash that will be finalized in commit.
// These writes will be persisted to the root multi-store
// (s.sm.CommitMultiStore()) and flushed
// to disk in the Commit phase. This means when the ABCI client requests
// Commit(), the application state transitions will be flushed to disk and as a
// result, but we already have
// an application Merkle root.
func (s *Service[LoggerT]) workingHash() []byte {
	// Write the FinalizeBlock state into branched storage and commit the
	// MultiStore. The write to the FinalizeBlock state writes all state
	// transitions to the root MultiStore (s.sm.CommitMultiStore())
	// so when Commit() is called it persists those values.
	if s.finalizeBlockState == nil {
		// this is unexpected since workingHash is called only after
		// internalFinalizeBlock. Panic appeases nilaway.
		panic(fmt.Errorf("workingHash: %w", errNilFinalizeBlockState))
	}
	s.finalizeBlockState.ms.Write()

	// Get the hash of all writes in order to return the apphash to the comet in
	// finalizeBlock.
	commitHash := s.sm.CommitMultiStore().WorkingHash()
	s.logger.Debug(
		"hash of all writes",
		"workingHash",
		fmt.Sprintf("%X", commitHash),
	)

	return commitHash
}

// getContextForProposal returns the correct Context for PrepareProposal and
// ProcessProposal. We use finalizeBlockState on the first block to be able to
// access any state changes made in InitChain.
func (s *Service[LoggerT]) getContextForProposal(
	ctx sdk.Context,
	height int64,
) sdk.Context {
	if height != s.initialHeight {
		return ctx
	}

	if s.finalizeBlockState == nil {
		// this is unexpected since cometBFT won't call PrepareProposal
		// on initialHeight. Panic appeases nilaway.
		panic(fmt.Errorf("getContextForProposal: %w", errNilFinalizeBlockState))
	}
	ctx, _ = s.finalizeBlockState.Context().CacheContext()
	return ctx
}

// CreateQueryContext creates a new sdk.Context for a query, taking as args
// the block height and whether the query needs a proof or not.
func (s *Service[LoggerT]) CreateQueryContext(
	height int64,
	prove bool,
) (sdk.Context, error) {
	// use custom query multi-store if provided
	lastBlockHeight := s.sm.CommitMultiStore().LatestVersion()
	if lastBlockHeight == 0 {
		return sdk.Context{}, errorsmod.Wrapf(
			sdkerrors.ErrInvalidHeight,
			"%s is not ready; please wait for first block",
			appName,
		)
	}

	if height > lastBlockHeight {
		return sdk.Context{},
			errorsmod.Wrap(
				sdkerrors.ErrInvalidHeight,
				"cannot query with height in the future; please provide a valid height",
			)
	}

	// when a client did not provide a query height, manually inject the latest
	if height == 0 {
		height = lastBlockHeight
	}

	if height <= 1 && prove {
		return sdk.Context{},
			errorsmod.Wrap(
				sdkerrors.ErrInvalidRequest,
				"cannot query with proof when height <= 1; please provide a valid height",
			)
	}

	cacheMS, err := s.sm.CommitMultiStore().CacheMultiStoreWithVersion(height)
	if err != nil {
		return sdk.Context{},
			errorsmod.Wrapf(
				sdkerrors.ErrNotFound,
				"failed to load state at height %d; %s (latest height: %d)",
				height,
				err,
				lastBlockHeight,
			)
	}

	return sdk.NewContext(
		cacheMS,
		true,
		servercmtlog.WrapSDKLogger(s.logger),
	), nil
}

// GetBlockRetentionHeight returns the height for which all blocks below this
// height
// are pruned from CometBFT. Given a commitment height and a non-zero local
// minRetainBlocks configuration, the retentionHeight is the smallest height
// that
// satisfies:
//
// - Unbonding (safety threshold) time: The block interval in which validators
// can be economically punished for misbehavior. Blocks in this interval must be
// auditable e.g. by the light client.
//
// - Logical store snapshot interval: The block interval at which the underlying
// logical store database is persisted to disk, e.g. every 10000 heights. Blocks
// since the last IAVL snapshot must be available for replay on application
// restart.
//
// - State sync snapshots: Blocks since the oldest available snapshot must be
// available for state sync nodes to catch up (oldest because a node may be
// restoring an old snapshot while a new snapshot was taken).
//
// - Local (minRetainBlocks) config: Archive nodes may want to retain more or
// all blocks, e.g. via a local config option min-retain-blocks. There may also
// be a need to vary retention for other nodes, e.g. sentry nodes which do not
// need historical blocks.
func (s *Service[_]) GetBlockRetentionHeight(commitHeight int64) int64 {
	// pruning is disabled if minRetainBlocks is zero
	if s.minRetainBlocks == 0 {
		return 0
	}

	minNonZero := func(x, y int64) int64 {
		switch {
		case x == 0:
			return y

		case y == 0:
			return x

		case x < y:
			return x

		default:
			return y
		}
	}

	// Define retentionHeight as the minimum value that satisfies all non-zero
	// constraints. All blocks below (commitHeight-retentionHeight) are pruned
	// from CometBFT.
	var retentionHeight int64

	// Define the number of blocks needed to protect against misbehaving
	// validators
	// which allows light clients to operate safely. Note, we piggy back of the
	// evidence parameters instead of computing an estimated number of blocks
	// based
	// on the unbonding period and block commitment time as the two should be
	// equivalent.
	if s.finalizeBlockState == nil {
		return 0
	}
	cp := s.paramStore.Get()
	if cp.Evidence != nil && cp.Evidence.MaxAgeNumBlocks > 0 {
		retentionHeight = commitHeight - cp.Evidence.MaxAgeNumBlocks
	}

	//#nosec:G701 // bet.
	v := commitHeight - int64(s.minRetainBlocks)
	retentionHeight = minNonZero(retentionHeight, v)

	if retentionHeight <= 0 {
		// prune nothing in the case of a non-positive height
		return 0
	}

	return retentionHeight
}

package node

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pegnet/pegnetd/node/pegnet"

	"github.com/Factom-Asset-Tokens/factom"
	"github.com/pegnet/pegnet/modules/conversions"
	"github.com/pegnet/pegnet/modules/grader"
	"github.com/pegnet/pegnetd/config"
	"github.com/pegnet/pegnetd/fat/fat2"
	log "github.com/sirupsen/logrus"
)

func (d *Pegnetd) GetCurrentSync() uint32 {
	// Should be thread safe since we only have 1 routine writing to it
	return d.Sync.Synced
}

// DBlockSync iterates through dblocks and syncs the various chains
func (d *Pegnetd) DBlockSync(ctx context.Context) {
	retryPeriod := d.Config.GetDuration(config.DBlockSyncRetryPeriod)
OuterSyncLoop:
	for {
		if isDone(ctx) {
			return // If the user does ctl+c or something
		}

		// Fetch the current highest height
		heights := new(factom.Heights)
		err := heights.Get(d.FactomClient)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{}).Errorf("failed to fetch heights")
			time.Sleep(retryPeriod)
			continue // Loop will just keep retrying until factomd is reached
		}

		if d.Sync.Synced >= heights.DirectoryBlock {
			// We are currently synced, nothing to do. If we are above it, the factomd could
			// be rebooted
			time.Sleep(retryPeriod) // TODO: Should we have a separate polling period?
			continue
		}

		var totalDur time.Duration
		var iterations int

		begin := time.Now()
		for d.Sync.Synced < heights.DirectoryBlock {
			start := time.Now()
			hLog := log.WithFields(log.Fields{"height": d.Sync.Synced + 1})
			if isDone(ctx) {
				return
			}

			// start transaction for all block actions
			tx, err := d.Pegnet.DB.BeginTx(ctx, nil)
			if err != nil {
				hLog.WithError(err).Errorf("failed to start transaction")
				continue
			}
			// We are not synced, so we need to iterate through the dblocks and sync them
			// one by one. We can only sync our current synced height +1
			// TODO: This skips the genesis block. I'm sure that is fine
			if err := d.SyncBlock(ctx, tx, d.Sync.Synced+1); err != nil {
				hLog.WithError(err).Errorf("failed to sync height")
				time.Sleep(retryPeriod)
				// If we fail, we backout to the outer loop. This allows error handling on factomd state to be a bit
				// cleaner, such as a rebooted node with a different db. That node would have a new heights response.
				err = tx.Rollback()
				if err != nil {
					// TODO evaluate if we can recover from this point or not
					hLog.WithError(err).Fatal("unable to roll back transaction")
				}
				continue OuterSyncLoop
			}

			// Bump our sync, and march forward

			d.Sync.Synced++
			err = d.Pegnet.InsertSynced(tx, d.Sync)
			if err != nil {
				d.Sync.Synced--
				hLog.WithError(err).Errorf("unable to update synced metadata")
				err = tx.Rollback()
				if err != nil {
					// TODO evaluate if we can recover from this point or not
					hLog.WithError(err).Fatal("unable to roll back transaction")
				}
				continue OuterSyncLoop
			}

			err = tx.Commit()
			if err != nil {
				d.Sync.Synced--
				hLog.WithError(err).Errorf("unable to commit transaction")
				err = tx.Rollback()
				if err != nil {
					// TODO evaluate if we can recover from this point or not
					hLog.WithError(err).Fatal("unable to roll back transaction")
				}
			}

			elapsed := time.Since(start)
			hLog.WithFields(log.Fields{"took": elapsed}).Debugf("synced")

			iterations++
			totalDur += elapsed
			// Only print if we are > 50 behind and every 50
			if iterations%50 == 0 {
				toGo := heights.DirectoryBlock - d.Sync.Synced
				avg := totalDur / time.Duration(iterations)
				hLog.WithFields(log.Fields{
					"avg":        avg,
					"left":       time.Duration(toGo) * avg,
					"syncing-to": heights.DirectoryBlock,
					"elapsed":    time.Since(begin),
				}).Infof("sync stats")
			}
		}

	}

}

// If SyncBlock returns no error, than that height was synced and saved. If any part of the sync fails,
// the whole sync should be rolled back and not applied. An error should then be returned.
// The context should be respected if it is cancelled
func (d *Pegnetd) SyncBlock(ctx context.Context, tx *sql.Tx, height uint32) error {
	fLog := log.WithFields(log.Fields{"height": height})
	if isDone(ctx) { // Just an example about how to handle it being cancelled
		return context.Canceled
	}

	dblock := new(factom.DBlock)
	dblock.Height = height
	if err := dblock.Get(d.FactomClient); err != nil {
		return err
	}

	// First, gather all entries we need from factomd
	oprEBlock := dblock.EBlock(OPRChain)
	if oprEBlock != nil {
		if err := multiFetch(oprEBlock, d.FactomClient); err != nil {
			return err
		}
	}
	transactionsEBlock := dblock.EBlock(TransactionChain)
	if transactionsEBlock != nil {
		if err := multiFetch(transactionsEBlock, d.FactomClient); err != nil {
			return err
		}
	}

	// Then, grade the new OPR Block. The results of this will be used
	// to execute conversions that are in holding.
	gradedBlock, err := d.Grade(ctx, oprEBlock)
	if err != nil {
		return err
	} else if gradedBlock != nil {
		err = d.Pegnet.InsertGradeBlock(tx, oprEBlock, gradedBlock)
		if err != nil {
			return err
		}
		winners := gradedBlock.Winners()
		if 0 < len(winners) {
			err = d.Pegnet.InsertRate(tx, height, winners[0].OPR.GetOrderedAssetsUint())
			if err != nil {
				return err
			}
		} else {
			fLog.WithFields(log.Fields{"section": "grading"}).Tracef("no winners")
		}
	}

	// At this point, we start making updates to the database in a specific order:
	// TODO: ensure we rollback the tx when needed
	// 1) Apply transaction batches that are in holding (conversions are always applied here)
	if gradedBlock != nil && 0 < len(gradedBlock.Winners()) {
		if err = d.ApplyTransactionBatchesInHolding(ctx, tx, height); err != nil {
			return err
		}
	}

	// 2) Sync transactions in current height and apply transactions
	if transactionsEBlock != nil {
		if err = d.ApplyTransactionBlock(tx, transactionsEBlock); err != nil {
			return err
		}
	}

	// 3) Apply FCT --> pFCT burns that happened in this block
	//    These funds will be available for transactions and conversions executed in the next block
	// TODO: Check the order of operations on this and what block to add burns from.
	if err := d.ApplyFactoidBlock(ctx, tx, dblock); err != nil {
		return err
	}

	// 4) Apply effects of graded OPR Block (PEG rewards, if any)
	//    These funds will be available for transactions and conversions executed in the next block
	if gradedBlock != nil {
		if err := d.ApplyGradedOPRBlock(tx, gradedBlock); err != nil {
			return err
		}
	}
	return nil
}

func multiFetch(eblock *factom.EBlock, c *factom.Client) error {
	err := eblock.Get(c)
	if err != nil {
		return err
	}

	work := make(chan int, len(eblock.Entries))
	defer close(work)
	errs := make(chan error)
	defer close(errs)

	for i := 0; i < 8; i++ {
		go func() {
			for j := range work {
				errs <- eblock.Entries[j].Get(c)
			}
		}()
	}

	for i := range eblock.Entries {
		work <- i
	}

	count := 0
	for e := range errs {
		count++
		if e != nil {
			return e
		}
		if count == len(eblock.Entries) {
			break
		}
	}

	return nil
}

// ApplyTransactionBatchesInHolding attempts to apply the transaction batches from previous
// blocks that were put into holding because they contained conversions.
// If an error is returned, the sql.Tx should be rolled back by the caller.
func (d *Pegnetd) ApplyTransactionBatchesInHolding(ctx context.Context, sqlTx *sql.Tx, currentHeight uint32) error {
	_, height, err := d.Pegnet.SelectMostRecentRatesBeforeHeight(ctx, sqlTx, currentHeight)
	if err != nil {
		return err
	}

	rates, err := d.Pegnet.SelectPendingRates(ctx, sqlTx, currentHeight)
	if err != nil {
		return err
	}

	for i := height; i < currentHeight; i++ {
		txBatches, err := d.Pegnet.SelectTransactionBatchesInHoldingAtHeight(uint64(i))
		if err != nil {
			return err
		}
		for _, txBatch := range txBatches {
			// Re-validate transaction batch because timestamp might not be valid anymore
			if err := txBatch.Validate(); err != nil {
				continue
			}
			isReplay, err := d.Pegnet.IsReplayTransaction(sqlTx, txBatch.Hash)
			if err != nil {
				return err
			} else if isReplay {
				continue
			}

			err = d.applyTransactionBatch(sqlTx, txBatch, rates, currentHeight)
			if err != nil && err != pegnet.InsufficientBalanceErr {
				return nil
			}
		}
	}
	return nil
}

// ApplyTransactionBlock puts conversion-containing transaction batches into holding,
// and applys the balance updates for all transaction batches able to be executed
// immediately. If an error is returned, the sql.Tx should be rolled back by the caller.
func (d *Pegnetd) ApplyTransactionBlock(sqlTx *sql.Tx, eblock *factom.EBlock) error {
	for _, entry := range eblock.Entries {
		txBatch := fat2.NewTransactionBatch(entry)
		err := txBatch.UnmarshalEntry()
		if err != nil {
			continue // Bad formatted entry
		}
		if err := txBatch.Validate(); err != nil {
			continue
		}
		log.WithFields(log.Fields{
			"height":    eblock.Height,
			"entryhash": entry.Hash.String(),
			"txs":       len(txBatch.Transactions)}).Tracef("tx found")

		isReplay, err := d.Pegnet.IsReplayTransaction(sqlTx, txBatch.Hash)
		if err != nil {
			return err
		} else if isReplay {
			continue
		}
		// At this point, we know that the transaction batch is valid and able to be executed.

		// A transaction batch that contains conversions must be put into holding to be executed
		// in a future block. This prevents gaming of conversions where an actor
		// can know the exchange rates of the future ahead of time.
		if txBatch.HasConversions() {
			_, err = d.Pegnet.InsertTransactionBatchHolding(sqlTx, txBatch, uint64(eblock.Height), eblock.KeyMR)
			if err != nil {
				return err
			}
			continue
		}

		// No conversions in the batch, it can be applied immediately
		if err = d.applyTransactionBatch(sqlTx, txBatch, nil, eblock.Height); err != nil && err != pegnet.InsufficientBalanceErr {
			return err
		}
	}
	return nil
}

// applyTransactionBatch
//	currentHeight is just for tracing
func (d *Pegnetd) applyTransactionBatch(sqlTx *sql.Tx, txBatch *fat2.TransactionBatch, rates map[fat2.PTicker]uint64, currentHeight uint32) error {
	for txIndex, tx := range txBatch.Transactions {
		var inputAdrID int64
		inputAdrID, txErr, err := d.Pegnet.SubFromBalance(sqlTx, &tx.Input.Address, tx.Input.Type, tx.Input.Amount)
		if err != nil {
			return err
		} else if txErr != nil {
			return txErr
		}
		_, err = d.Pegnet.InsertTransactionRelation(sqlTx, inputAdrID, txBatch.Hash, uint64(txIndex), false, tx.IsConversion())
		if err != nil {
			return err
		}

		if tx.IsConversion() {
			if rates == nil || len(rates) == 0 {
				return fmt.Errorf("rates must exist if TransactionBatch contains conversions")
			}
			if rates[tx.Input.Type] == 0 || rates[tx.Conversion] == 0 {
				return nil // 0 rates result in an invalid tx. So we drop it
			}
			outputAmount, err := conversions.Convert(int64(tx.Input.Amount), rates[tx.Input.Type], rates[tx.Conversion])
			if err != nil {
				return err
			}
			_, err = d.Pegnet.AddToBalance(sqlTx, &tx.Input.Address, tx.Conversion, uint64(outputAmount))
			if err != nil {
				return err
			}
		} else {
			for _, transfer := range tx.Transfers {
				var outputAdrID int64
				outputAdrID, err = d.Pegnet.AddToBalance(sqlTx, &transfer.Address, tx.Input.Type, transfer.Amount)
				if err != nil {
					return err
				}
				_, err = d.Pegnet.InsertTransactionRelation(sqlTx, outputAdrID, txBatch.Hash, uint64(txIndex), true, false)
				if err != nil {
					return err
				}
			}
		}
	}
	log.WithFields(log.Fields{
		"height":     currentHeight, // Just for log traces
		"entryhash":  txBatch.Hash.String(),
		"conversion": txBatch.HasConversions(),
		"txs":        len(txBatch.Transactions)}).Tracef("tx applied")

	return nil
}

// ApplyFactoidBlock applies the FCT burns that occurred within the given
// DBlock. If an error is returned, the sql.Tx should be rolled back by the caller.
func (d *Pegnetd) ApplyFactoidBlock(ctx context.Context, tx *sql.Tx, dblock *factom.DBlock) error {
	fblock := new(factom.FBlock)
	fblock.Header.Height = dblock.Height
	if err := fblock.Get(d.FactomClient); err != nil {
		return err
	}

	var totalBurned uint64
	var burns []factom.FactoidTransactionIO

	// Register all burns. Burns have a few requirements
	// - Only 1 output, and that output must be the EC burn address
	// - The output amount must be 0
	// - Must only have 1 input
	for i := range fblock.Transactions {
		if isDone(ctx) {
			return context.Canceled
		}

		if err := fblock.Transactions[i].Get(d.FactomClient); err != nil {
			return err
		}

		tx := fblock.Transactions[i]
		// Check number of inputs/outputs
		if len(tx.ECOutputs) != 1 || len(tx.FCTInputs) != 1 || len(tx.FCTOutputs) > 0 {
			continue // Wrong number of ins/outs for a burn
		}

		// Check correct output
		out := tx.ECOutputs[0]
		if BurnRCD != *out.Address {
			continue // Wrong EC output for a burn
		}

		// Check right output amount
		if out.Amount != 0 {
			continue // You cannot buy EC and burn
		}

		in := tx.FCTInputs[0]
		totalBurned += in.Amount
		burns = append(burns, in)
	}

	var _ = burns
	if totalBurned > 0 { // Just some debugging
		log.WithFields(log.Fields{"height": dblock.Height, "amount": totalBurned, "quantity": len(burns)}).Debug("fct burned")
	}

	// All burns are FCT inputs
	for i := range burns {
		var add factom.FAAddress
		copy(add[:], burns[i].Address[:])
		if _, err := d.Pegnet.AddToBalance(tx, &add, fat2.PTickerFCT, burns[i].Amount); err != nil {
			return err
		}
	}

	return nil
}

// ApplyGradedOPRBlock pays out PEG to the winners of the given GradedBlock.
// If an error is returned, the sql.Tx should be rolled back by the caller.
func (d *Pegnetd) ApplyGradedOPRBlock(tx *sql.Tx, gradedBlock grader.GradedBlock) error {
	winners := gradedBlock.Winners()
	for i := range winners {
		addr, err := factom.NewFAAddress(winners[i].OPR.GetAddress())
		if err != nil {
			// TODO: This is kinda an odd case. I think we should just drop the rewards
			// 		for an invalid address. We can always add back the rewards and they will have
			//		a higher balance after a change.
			log.WithError(err).WithFields(log.Fields{
				"height": winners[i].OPR.GetHeight(),
				"ehash":  fmt.Sprintf("%x", winners[i].EntryHash),
			}).Warnf("failed to reward")
			continue
		}

		if _, err := d.Pegnet.AddToBalance(tx, &addr, fat2.PTickerPEG, uint64(winners[i].Payout())); err != nil {
			return err
		}
	}
	return nil
}

func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

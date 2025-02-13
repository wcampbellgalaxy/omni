package appv2

import (
	"context"

	"github.com/omni-network/omni/lib/errors"
	"github.com/omni-network/omni/lib/log"
	"github.com/omni-network/omni/lib/xchain"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// newEventProcessor returns a callback provided to xchain.Provider::StreamEventLogs processing
// all inbox contract events and driving order lifecycle.
func newEventProcessor(deps procDeps, chainID uint64) xchain.EventLogsCallback {
	return func(ctx context.Context, height uint64, elogs []types.Log) error {
		// TODO: do return error on for an error on a single log. this skips potentially valid logs in the same block.

		for _, elog := range elogs {
			event, ok := eventsByTopic[elog.Topics[0]]
			if !ok {
				return errors.New("unknown event [BUG]")
			}

			orderID, err := deps.ParseID(chainID, elog)
			if err != nil {
				return errors.Wrap(err, "parse id")
			}

			order, found, err := deps.GetOrder(ctx, chainID, orderID)
			if err != nil {
				return errors.Wrap(err, "get order")
			} else if !found {
				return errors.New("order not found [BUG]")
			}

			target := deps.TargetName(order)
			statusOffset.WithLabelValues(deps.ChainName(chainID), target, statusString(event.Status)).Set(float64(orderID.Uint64()))

			attrs := []any{
				"id", order.ID.String(),
				"status", order.Status,
				"src_chain_id", order.SourceChainID,
				"dst_chain_id", order.DestinationChainID,
			}

			fill, err := order.ParsedFillOriginData()
			if err != nil {
				log.Warn(ctx, "Failed to parse fill origin data", err, attrs...)
				attrs = append(attrs,
					"call_target", unknown,
					"call_value", unknown,
					"call_data", unknown,
				)
			} else {
				attrs = append(attrs,
					"call_target", hexutil.Encode(fill.Call.Target[:]),
					"call_value", fill.Call.Value.Uint64(),
					"call_data", hexutil.Encode(fill.Call.Data),
				)
			}

			ctx := log.WithCtx(ctx, attrs...)

			log.Debug(ctx, "Processing order event")

			if event.Status != order.Status {
				// TODO(corver): Detect unexpected on-chain status.
				log.Info(ctx, "Ignoring mismatching old event", "actual", statusString(order.Status))
				continue
			}

			switch event.Status {
			case statusPending:
				if reason, reject, err := deps.ShouldReject(ctx, chainID, order); err != nil {
					return errors.Wrap(err, "should reject")
				} else if reject {
					// ShouldReject does reject logging since it has more information.
					if err := deps.Reject(ctx, chainID, order, reason); err != nil {
						return errors.Wrap(err, "reject order")
					}
				} else {
					log.Info(ctx, "Accepting order")
					if err := deps.Accept(ctx, chainID, order); err != nil {
						return errors.Wrap(err, "accept order")
					}
				}
			case statusAccepted:
				log.Info(ctx, "Filling order")
				if err := deps.Fill(ctx, chainID, order); err != nil {
					return errors.Wrap(err, "fill order")
				}
			case statusFilled:
				log.Info(ctx, "Claiming order")
				if err := deps.Claim(ctx, chainID, order); err != nil {
					return errors.Wrap(err, "claim order")
				}
			case statusRejected, statusReverted, statusClaimed:
			// Ignore for now
			default:
				return errors.New("unknown status [BUG]")
			}

			processedEvents.WithLabelValues(deps.ChainName(chainID), target, statusString(event.Status)).Inc()
		}

		return deps.SetCursor(ctx, chainID, height)
	}
}

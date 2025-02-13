package appv2

import (
	"context"

	"github.com/omni-network/omni/lib/errors"
	"github.com/omni-network/omni/lib/ethclient/ethbackend"
	"github.com/omni-network/omni/lib/log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

//go:generate stringer -type=rejectReason -trimprefix=reject
type rejectReason uint8

const (
	rejectNone                  rejectReason = 0
	rejectDestCallReverts       rejectReason = 1
	rejectInsufficientFee       rejectReason = 2
	rejectInsufficientInventory rejectReason = 3
	rejectUnsupportedToken      rejectReason = 4
	rejectUnsupportedDestChain  rejectReason = 5
)

// newShouldRejector returns as ShouldReject function for the given network.
//
// ShouldReject returns true and a reason if the request should be rejected.
// It returns false if the request should be accepted.
// Errors are unexpected and refer to internal server problems.
func newShouldRejector(
	backends ethbackend.Backends,
	solverAddr common.Address,
	targetName func(Order) string,
	chainName func(uint64) string,
) func(ctx context.Context, srcChainID uint64, order Order) (rejectReason, bool, error) {
	return func(ctx context.Context, srcChainID uint64, order Order) (rejectReason, bool, error) {
		// reject swallows the error (only logging it) and returns true and the shouldReject reason.
		reject := func(reason rejectReason, err error) (rejectReason, bool, error) {
			err = errors.Wrap(err, "reject",
				"order_id", order.ID.String(),
				"dest_chain_id", order.DestinationChainID,
				"src_chain_id", order.SourceChainID,
				"target", targetName(order))

			rejectedOrders.WithLabelValues(
				chainName(order.SourceChainID),
				chainName(order.DestinationChainID),
				targetName(order),
				reason.String(),
			).Inc()

			log.InfoErr(ctx, "Rejecting request", err, "reason", reason)

			return reason, true, nil
		}

		// returnErr returns the error, with false and rejectNone. It should be used for unexpected errors.
		returnErr := func(err error) (rejectReason, bool, error) {
			return rejectNone, false, err
		}

		if srcChainID != order.SourceChainID {
			return returnErr(errors.New("source chain id mismatch [BUG]", "got", order.SourceChainID, "expected", srcChainID))
		}

		destChainID := order.DestinationChainID
		backend, err := backends.Backend(destChainID)
		if err != nil {
			return reject(rejectUnsupportedDestChain, err)
		}

		// check all expenses are supported, ethereum addressed tokens
		var expenses []Expense
		for _, output := range order.MaxSpent {
			chainID := output.ChainId.Uint64()

			// inbox contract order resolution should ensure output.chainId matches order.DestinationChainID (derived from fillInstructions)
			if chainID != destChainID {
				return returnErr(errors.New("max spent chain id mismatch [BUG]", "got", chainID, "expected", destChainID))
			}

			addr := toEthAddr(output.Token)
			if !cmpAddrs(addr, output.Token) {
				return reject(rejectUnsupportedToken, errors.New("non-eth addressed token", "addr", hexutil.Encode(output.Token[:])))
			}

			tkn, ok := tokens.find(chainID, addr)
			if !ok {
				return reject(rejectUnsupportedToken, errors.New("unsupported token", "addr", addr))
			}

			expenses = append(expenses, Expense{
				token:  tkn,
				amount: output.Amount,
			})
		}

		// check liquidty, reject if insufficient
		for _, expense := range expenses {
			bal, err := balanceOf(ctx, expense.token, backend, solverAddr)
			if err != nil {
				return returnErr(errors.Wrap(err, "get balance", "token", expense.token.Symbol))
			}

			// TODO: for native tokens, even if we have enough, we don't want to
			// spend out whole balance. we'll need to keep some for gas
			if bal.Cmp(expense.amount) < 0 {
				return reject(rejectInsufficientInventory, errors.New("insufficient balance", "token", expense.token.Symbol))
			}
		}

		return rejectNone, false, nil
	}
}

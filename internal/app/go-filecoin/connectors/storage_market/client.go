package storagemarketconnector

import (
	"context"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-fil-markets/shared"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	spaminer "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	spapow "github.com/filecoin-project/specs-actors/actors/builtin/power"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-hamt-ipld"
	cbor "github.com/ipfs/go-ipld-cbor"
	xerrors "github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/internal/app/go-filecoin/connectors"
	"github.com/filecoin-project/go-filecoin/internal/app/go-filecoin/plumbing/msg"
	"github.com/filecoin-project/go-filecoin/internal/pkg/block"
	"github.com/filecoin-project/go-filecoin/internal/pkg/encoding"
	"github.com/filecoin-project/go-filecoin/internal/pkg/message"
	appstate "github.com/filecoin-project/go-filecoin/internal/pkg/state"
	"github.com/filecoin-project/go-filecoin/internal/pkg/types"
)

// StorageClientNodeConnector adapts the node to provide the correct interface to the storage client.
type StorageClientNodeConnector struct {
	connectorCommon

	clientAddr address.Address
	cborStore  cbor.IpldStore
}

var _ storagemarket.StorageClientNode = &StorageClientNodeConnector{}

// NewStorageClientNodeConnector creates a new connector
func NewStorageClientNodeConnector(
	cbor cbor.IpldStore,
	cs chainReader,
	w *msg.Waiter,
	s types.Signer,
	ob *message.Outbox,
	ca address.Address,
	sv *appstate.Viewer,
) *StorageClientNodeConnector {
	return &StorageClientNodeConnector{
		connectorCommon: connectorCommon{cs, sv, w, s, ob},
		cborStore:       cbor,
		clientAddr:      ca,
	}
}

// AddFunds sends a message to add collateral for the given address
func (s *StorageClientNodeConnector) AddFunds(ctx context.Context, addr address.Address, amount abi.TokenAmount) error {
	return s.addFunds(ctx, s.clientAddr, addr, amount)
}

// EnsureFunds checks the current balance for an address and adds funds if the balance is below the given amount
func (s *StorageClientNodeConnector) EnsureFunds(ctx context.Context, addr, walletAddr address.Address, amount abi.TokenAmount, tok shared.TipSetToken) error {
	balance, err := s.GetBalance(ctx, addr, tok)
	if err != nil {
		return err
	}

	if !balance.Available.LessThan(amount) {
		// TODO: Transfer funds to the market actor on behalf of `addr`
		return nil
	}

	return s.AddFunds(ctx, addr, big.Sub(amount, balance.Available))
}

// ListClientDeals returns all deals published on chain for the given account
func (s *StorageClientNodeConnector) ListClientDeals(ctx context.Context, addr address.Address, tok shared.TipSetToken) ([]storagemarket.StorageDeal, error) {
	var tsk block.TipSetKey
	if err := encoding.Decode(tok, &tsk); err != nil {
		return nil, xerrors.Errorf("failed to marshal TipSetToken into a TipSetKey: %w", err)
	}

	return s.listDeals(ctx, addr, tsk)
}

// ListStorageProviders finds all miners that will provide storage
func (s *StorageClientNodeConnector) ListStorageProviders(ctx context.Context, tok shared.TipSetToken) ([]*storagemarket.StorageProviderInfo, error) {
	var tsk block.TipSetKey
	if err := encoding.Decode(tok, &tsk); err != nil {
		return nil, xerrors.Errorf("failed to marshal TipSetToken into a TipSetKey: %w", err)
	}

	var spState spapow.State
	err := s.chainStore.GetActorStateAt(ctx, tsk, builtin.StoragePowerActorAddr, &spState)
	if err != nil {
		return nil, err
	}

	infos := []*storagemarket.StorageProviderInfo{}
	powerHamt, err := hamt.LoadNode(ctx, s.cborStore, spState.Claims)
	if err != nil {
		return nil, err
	}

	err = powerHamt.ForEach(ctx, func(minerAddrStr string, _ interface{}) error {
		minerAddr, err := address.NewFromString(minerAddrStr)
		if err != nil {
			return err
		}

		var mState spaminer.State
		err = s.chainStore.GetActorStateAt(ctx, tsk, minerAddr, &mState)
		if err != nil {
			return err
		}

		info := mState.Info
		infos = append(infos, &storagemarket.StorageProviderInfo{
			Address:    minerAddr,
			Owner:      info.Owner,
			Worker:     info.Worker,
			SectorSize: uint64(info.SectorSize),
			PeerID:     info.PeerId,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	return infos, nil
}

// ValidatePublishedDeal validates a deal has been published correctly
// Adapted from https://github.com/filecoin-project/lotus/blob/3b34eba6124d16162b712e971f0db2ee108e0f67/markets/storageadapter/client.go#L156
func (s *StorageClientNodeConnector) ValidatePublishedDeal(ctx context.Context, deal storagemarket.ClientDeal) (dealID abi.DealID, err error) {
	// Fetch receipt to return dealId
	chnMsg, found, err := s.waiter.Find(ctx, func(msg *types.SignedMessage, c cid.Cid) bool {
		return c.Equals(*deal.PublishMessage)
	})
	if err != nil {
		return 0, err
	}

	if !found {
		return 0, xerrors.Errorf("Could not find published deal message %s", deal.PublishMessage.String())
	}

	unsigned := chnMsg.Message.Message

	minerWorker, err := s.GetMinerWorkerAddress(ctx, deal.Proposal.Provider, nil)
	if err != nil {
		return 0, err
	}

	if unsigned.From != minerWorker {
		return 0, xerrors.Errorf("deal wasn't published by storage provider: from=%s, provider=%s", unsigned.From, deal.Proposal.Provider)
	}

	if unsigned.To != builtin.StorageMarketActorAddr {
		return 0, xerrors.Errorf("deal publish message wasn't set to StorageMarket actor (to=%s)", unsigned.To)
	}

	if unsigned.Method != builtin.MethodsMarket.PublishStorageDeals {
		return 0, xerrors.Errorf("deal publish message called incorrect method (method=%d)", unsigned.Method)
	}

	var params market.PublishStorageDealsParams
	err = encoding.Decode(unsigned.Params, &params)
	if err != nil {
		return 0, err
	}

	msgProposals := params.Deals
	// The return value doesn't recapitulate the whole deal. If inspection is required, we should look up the deal
	// in the market actor state.

	for _, proposal := range msgProposals {
		if proposal.Proposal == deal.Proposal {
			var ret market.PublishStorageDealsReturn
			err := encoding.Decode(chnMsg.Receipt.ReturnValue, &ret)
			if err != nil {
				return 0, err
			}
			return ret.IDs[0], nil
		}
	}

	return 0, xerrors.Errorf("published deal does not match ClientDeal")
}

// SignProposal uses the local wallet to sign the given proposal
func (s *StorageClientNodeConnector) SignProposal(ctx context.Context, signer address.Address, proposal market.DealProposal) (*market.ClientDealProposal, error) {
	buf, err := encoding.Encode(proposal)
	if err != nil {
		return nil, err
	}

	signature, err := s.SignBytes(ctx, signer, buf)
	if err != nil {
		return nil, err
	}

	return &market.ClientDealProposal{
		Proposal:        proposal,
		ClientSignature: *signature,
	}, nil
}

// GetDefaultWalletAddress returns the default account for this node
func (s *StorageClientNodeConnector) GetDefaultWalletAddress(ctx context.Context) (address.Address, error) {
	return s.clientAddr, nil
}

// ValidateAskSignature ensures the given ask has been signed correctly
func (s *StorageClientNodeConnector) ValidateAskSignature(ctx context.Context, signed *storagemarket.SignedStorageAsk, tok shared.TipSetToken) (bool, error) {
	ask := signed.Ask

	buf, err := encoding.Encode(ask)
	if err != nil {
		return false, err
	}

	return s.VerifySignature(ctx, *signed.Signature, ask.Miner, buf, tok)
}

func (s *StorageClientNodeConnector) GetChainHead(ctx context.Context) (shared.TipSetToken, abi.ChainEpoch, error) {
	return connectors.GetChainHead(s.chainStore)
}

func (s *StorageClientNodeConnector) OnDealSectorCommitted(ctx context.Context, provider address.Address, dealID abi.DealID, cb storagemarket.DealSectorCommittedCallback) error {
	panic("implement me") // @laser to do this
}

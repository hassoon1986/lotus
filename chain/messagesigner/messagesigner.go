package messagesigner

import (
	"bytes"
	"context"
	"sync"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/chain/messagepool"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/wallet"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	logging "github.com/ipfs/go-log/v2"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"
)

const dsKeyActorNonce = "ActorNonce"

var log = logging.Logger("messagesigner")

type mpoolAPI interface {
	GetNonce(address.Address) (uint64, error)
}

// MessageSigner keeps track of nonces per address, and increments the nonce
// when signing a message
type MessageSigner struct {
	wallet *wallet.Wallet
	lk     sync.Mutex
	mpool  mpoolAPI
	ds     datastore.Batching
}

func NewMessageSigner(wallet *wallet.Wallet, mpool *messagepool.MessagePool, ds dtypes.MetadataDS) *MessageSigner {
	return newMessageSigner(wallet, mpool, ds)
}

func newMessageSigner(wallet *wallet.Wallet, mpool mpoolAPI, ds dtypes.MetadataDS) *MessageSigner {
	ds = namespace.Wrap(ds, datastore.NewKey("/message-signer/"))
	return &MessageSigner{
		wallet: wallet,
		mpool:  mpool,
		ds:     ds,
	}
}

// SignMessage increments the nonce for the message From address, and signs
// the message
func (ms *MessageSigner) SignMessage(ctx context.Context, msg *types.Message, cb func(*types.SignedMessage) error) (*types.SignedMessage, error) {
	ms.lk.Lock()
	defer ms.lk.Unlock()

	// Get the next message nonce
	nonce, err := ms.nextNonce(msg.From)
	if err != nil {
		return nil, xerrors.Errorf("failed to create nonce: %w", err)
	}

	// Sign the message with the nonce
	msg.Nonce = nonce
	sig, err := ms.wallet.Sign(ctx, msg.From, msg.Cid().Bytes())
	if err != nil {
		return nil, xerrors.Errorf("failed to sign message: %w", err)
	}

	// Callback with the signed message
	smsg := &types.SignedMessage{
		Message:   *msg,
		Signature: *sig,
	}
	err = cb(smsg)
	if err != nil {
		return nil, err
	}

	// If the callback executed successfully, write the nonce to the datastore
	if err := ms.saveNonce(msg.From, nonce); err != nil {
		return nil, xerrors.Errorf("failed to save nonce: %w", err)
	}

	return smsg, nil
}

// nextNonce increments the nonce.
// If there is no nonce in the datastore, gets the nonce from the message pool.
func (ms *MessageSigner) nextNonce(addr address.Address) (uint64, error) {
	// Nonces used to be created by the mempool and we need to support nodes
	// that have mempool nonces, so first check the mempool for a nonce for
	// this address. Note that the mempool returns the actor state's nonce
	// by default.
	nonce, err := ms.mpool.GetNonce(addr)
	if err != nil {
		return 0, xerrors.Errorf("failed to get nonce from mempool: %w", err)
	}

	// Get the nonce for this address from the datastore
	addrNonceKey := ms.dstoreKey(addr)
	dsNonceBytes, err := ms.ds.Get(addrNonceKey)

	switch {
	case xerrors.Is(err, datastore.ErrNotFound):
		// If a nonce for this address hasn't yet been created in the
		// datastore, just use the nonce from the mempool

	case err != nil:
		return 0, xerrors.Errorf("failed to get nonce from datastore: %w", err)

	default:
		// There is a nonce in the datastore, so unmarshall and increment it
		maj, val, err := cbg.CborReadHeader(bytes.NewReader(dsNonceBytes))
		if err != nil {
			return 0, xerrors.Errorf("failed to parse nonce from datastore: %w", err)
		}
		if maj != cbg.MajUnsignedInt {
			return 0, xerrors.Errorf("bad cbor type parsing nonce from datastore")
		}

		dsNonce := val + 1

		// The message pool nonce should be <= than the datastore nonce
		if nonce <= dsNonce {
			nonce = dsNonce
		} else {
			log.Warnf("mempool nonce was larger than datastore nonce (%d > %d)", nonce, dsNonce)
		}
	}

	return nonce, nil
}

// saveNonce writes the nonce for this address to the datastore
func (ms *MessageSigner) saveNonce(addr address.Address, nonce uint64) error {
	addrNonceKey := ms.dstoreKey(addr)
	buf := bytes.Buffer{}
	_, err := buf.Write(cbg.CborEncodeMajorType(cbg.MajUnsignedInt, nonce))
	if err != nil {
		return xerrors.Errorf("failed to marshall nonce: %w", err)
	}
	err = ms.ds.Put(addrNonceKey, buf.Bytes())
	if err != nil {
		return xerrors.Errorf("failed to write nonce to datastore: %w", err)
	}
	return nil
}

func (ms *MessageSigner) dstoreKey(addr address.Address) datastore.Key {
	return datastore.KeyWithNamespaces([]string{dsKeyActorNonce, addr.String()})
}

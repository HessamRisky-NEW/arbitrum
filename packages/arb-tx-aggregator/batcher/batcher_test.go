/*
 * Copyright 2020, Offchain Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package batcher

import (
	"context"
	"crypto/ecdsa"
	"errors"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/offchainlabs/arbitrum/packages/arb-evm/message"
	"github.com/offchainlabs/arbitrum/packages/arb-util/common"
	"github.com/offchainlabs/arbitrum/packages/arb-validator-core/arbbridge"
	"math/big"
	"math/rand"
	"testing"
	"time"
)

type mock struct {
	t            *testing.T
	sentL1Txes   map[common.Hash]bool
	seenTxesChan chan<- message.CompressedECDSATransaction
}

func newMock(t *testing.T, seenTxesChan chan<- message.CompressedECDSATransaction, txes []*types.Transaction) *mock {
	return &mock{
		t:            t,
		sentL1Txes:   make(map[common.Hash]bool),
		seenTxesChan: seenTxesChan,
	}
}

func (m *mock) SendL2MessageNoWait(_ context.Context, data []byte) (common.Hash, error) {
	l1Hash := common.RandHash()
	m.sentL1Txes[l1Hash] = true

	msg, err := message.L2Message{Data: data}.AbstractMessage()
	if err != nil {
		m.t.Error(err)
		return common.Hash{}, err
	}
	batch, ok := msg.(message.TransactionBatch)
	if !ok {
		m.t.Error("expected msg to be batch")
		return l1Hash, nil
	}
	for _, rawTx := range batch.Transactions {
		msg, err := message.L2Message{Data: rawTx}.AbstractMessage()
		if err != nil {
			m.t.Error(err)
			continue
		}
		compressedTx, ok := msg.(message.CompressedECDSATransaction)
		if !ok {
			m.t.Error("expected msg to be compressed ecdsa tx")
			continue
		}
		m.seenTxesChan <- compressedTx
	}
	return l1Hash, nil
}

func (m *mock) TransactionReceipt(_ context.Context, txHash ethcommon.Hash) (*types.Receipt, error) {
	_, ok := m.sentL1Txes[common.NewHashFromEth(txHash)]
	if !ok {
		return nil, errors.New("tx not sent")
	}
	return &types.Receipt{
		Status:      1,
		TxHash:      txHash,
		GasUsed:     0,
		BlockNumber: big.NewInt(0),
	}, nil
}

func (m *mock) SendL2Message(context.Context, []byte) (arbbridge.MessageDeliveredEvent, error) {
	panic("not used")
}

func (m *mock) DepositEthMessage(context.Context, common.Address, *big.Int) error {
	panic("not used")
}

func (m *mock) DepositERC20Message(context.Context, common.Address, common.Address, *big.Int) error {
	panic("not used")
}

func (m *mock) DepositERC721Message(context.Context, common.Address, common.Address, *big.Int) error {
	panic("not used")
}

func generateTxes(t *testing.T, chain common.Address) ([]*types.Transaction, map[ethcommon.Address]uint64) {
	rand.Seed(4537345)
	signer := types.NewEIP155Signer(message.ChainAddressToID(chain))
	randomKeys := make([]*ecdsa.PrivateKey, 0, 10)
	for i := 0; i < 10; i++ {
		pk, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		randomKeys = append(randomKeys, pk)
	}
	txCounts := make(map[ethcommon.Address]uint64)
	var txes []*types.Transaction
	for i := 0; i < 100; i++ {
		pk := randomKeys[rand.Intn(len(randomKeys))]
		sender := crypto.PubkeyToAddress(pk.PublicKey)
		tx := types.NewTransaction(txCounts[sender], ethcommon.Address{6}, big.NewInt(0), 1000, big.NewInt(10), nil)
		signedTx, err := types.SignTx(tx, signer, pk)
		if err != nil {
			t.Fatal(err)
		}
		txes = append(txes, signedTx)
		txCounts[sender]++
	}
	return txes, txCounts
}

func TestStatelessBatcher(t *testing.T) {
	chain := common.RandAddress()
	signer := types.NewEIP155Signer(message.ChainAddressToID(chain))
	txes, txCounts := generateTxes(t, chain)
	seenTxesChan := make(chan message.CompressedECDSATransaction, 1000)
	mock := newMock(t, seenTxesChan, txes)
	batcher := NewStatelessBatcher(
		context.Background(),
		chain,
		mock,
		mock,
		time.Millisecond*200,
	)

	for _, tx := range txes {
		_, err := batcher.SendTransaction(tx)
		if err != nil {
			t.Fatal(err)
		}
		<-time.After(time.Millisecond * 20)
	}

	sentL2Txes := make(map[ethcommon.Hash]bool)
	for _, tx := range txes {
		sentL2Txes[tx.Hash()] = true
	}

	txesBySender := make(map[ethcommon.Address][]*types.Transaction)
	seenTxCount := 0
txFetchLoop:
	for {
		select {
		case tx := <-seenTxesChan:
			ethTx, err := tx.AsEthTx(message.ChainAddressToID(chain))
			if err != nil {
				t.Fatal(err)
			}
			if !sentL2Txes[ethTx.Hash()] {
				jsonData, err := ethTx.MarshalJSON()
				if err != nil {
					t.Fatal(err)
				}
				t.Log(string(jsonData))
				t.Fatal("saw tx that wasn't sent")
			}
			sender, err := types.Sender(signer, ethTx)
			if err != nil {
				t.Fatal(err)
			}
			txesBySender[sender] = append(txesBySender[sender], ethTx)
			seenTxCount++
			if seenTxCount == len(txes) {
				break txFetchLoop
			}
		case <-time.After(time.Second * 2):
			t.Fatal("timed out waiting for txes")
		}
	}

	for sender, txes := range txesBySender {
		if txCounts[sender] != uint64(len(txes)) {
			t.Error("unexpected tx count from sender", txCounts[sender], len(txes))
		}
		for i, tx := range txes {
			if tx.Nonce() != uint64(i) {
				t.Error("unexpected nonce", tx.Nonce(), "instead of", i)
			}
		}
	}
}

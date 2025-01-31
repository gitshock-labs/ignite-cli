package cosmosclient_test

import (
	"bufio"
	"context"
	"encoding/hex"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/client/flags"
	sdktypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tendermint/tendermint/p2p"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"

	"github.com/ignite/cli/ignite/pkg/cosmosaccount"
	"github.com/ignite/cli/ignite/pkg/cosmosclient"
	"github.com/ignite/cli/ignite/pkg/cosmosclient/mocks"
	"github.com/ignite/cli/ignite/pkg/cosmosfaucet"
)

const (
	defaultFaucetDenom     = "token"
	defaultFaucetMinAmount = 100
)

//go:generate mockery --srcpkg github.com/tendermint/tendermint/rpc/client/ --name Client --structname RPCClient --filename rpclient.go --with-expecter
//go:generate mockery --srcpkg github.com/cosmos/cosmos-sdk/client --name AccountRetriever --filename account_retriever.go --with-expecter
//go:generate mockery --srcpkg github.com/cosmos/cosmos-sdk/x/bank/types --name QueryClient --structname BankQueryClient --filename bank_query_client.go --with-expecter
//go:generate mockery --srcpkg . --name FaucetClient --structname FaucetClient --filename faucet_client.go --with-expecter
//go:generate mockery --srcpkg . --name Gasometer --filename gasometer.go --with-expecter
//go:generate mockery --srcpkg . --name Signer --filename signer.go --with-expecter

type suite struct {
	rpcClient        *mocks.RPCClient
	accountRetriever *mocks.AccountRetriever
	bankQueryClient  *mocks.BankQueryClient
	gasometer        *mocks.Gasometer
	faucetClient     *mocks.FaucetClient
	signer           *mocks.Signer
}

func newClient(t *testing.T, setup func(suite), opts ...cosmosclient.Option) cosmosclient.Client {
	s := suite{
		rpcClient:        mocks.NewRPCClient(t),
		accountRetriever: mocks.NewAccountRetriever(t),
		bankQueryClient:  mocks.NewBankQueryClient(t),
		gasometer:        mocks.NewGasometer(t),
		faucetClient:     mocks.NewFaucetClient(t),
		signer:           mocks.NewSigner(t),
	}
	// Because rpcClient is passed as argument inside clientContext of mocked
	// methods, we must EXPECT a call to String (because testify/mock is calling
	// String() on mocked methods' args)
	s.rpcClient.EXPECT().String().Return("plop").Maybe()
	// cosmosclient.New always makes a call to Status
	s.rpcClient.EXPECT().Status(mock.Anything).
		Return(&ctypes.ResultStatus{
			NodeInfo: p2p.DefaultNodeInfo{Network: "mychain"},
		}, nil).Once()
	if setup != nil {
		setup(s)
	}
	opts = append(opts, []cosmosclient.Option{
		cosmosclient.WithKeyringBackend(cosmosaccount.KeyringMemory),
		cosmosclient.WithRPCClient(s.rpcClient),
		cosmosclient.WithAccountRetriever(s.accountRetriever),
		cosmosclient.WithBankQueryClient(s.bankQueryClient),
		cosmosclient.WithGasometer(s.gasometer),
		cosmosclient.WithFaucetClient(s.faucetClient),
		cosmosclient.WithSigner(s.signer),
	}...)
	c, err := cosmosclient.New(context.Background(), opts...)
	require.NoError(t, err)
	return c
}

func TestNew(t *testing.T) {
	assert := assert.New(t)

	c := newClient(t, nil)

	ctx := c.Context()
	assert.Equal("mychain", ctx.ChainID)
	assert.NotNil(ctx.InterfaceRegistry)
	assert.NotNil(ctx.Codec)
	assert.NotNil(ctx.TxConfig)
	assert.NotNil(ctx.LegacyAmino)
	assert.Equal(bufio.NewReader(os.Stdin), ctx.Input)
	assert.Equal(io.Discard, ctx.Output)
	assert.NotNil(ctx.AccountRetriever)
	assert.Equal(flags.BroadcastSync, ctx.BroadcastMode)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(home+"/.mychain", ctx.HomeDir)
	assert.NotNil(ctx.Client)
	assert.True(ctx.SkipConfirm)
	assert.Equal(c.AccountRegistry.Keyring, ctx.Keyring)
	assert.False(ctx.GenerateOnly)
	txf := c.TxFactory
	assert.Equal("mychain", txf.ChainID())
	assert.Equal(c.AccountRegistry.Keyring, txf.Keybase())
	assert.EqualValues(300000, txf.Gas())
	assert.Equal(1.0, txf.GasAdjustment())
	assert.Equal(signing.SignMode_SIGN_MODE_UNSPECIFIED, txf.SignMode())
	assert.NotNil(txf.AccountRetriever())
}

func TestClientWaitForBlockHeight(t *testing.T) {
	targetBlockHeight := int64(42)
	tests := []struct {
		name          string
		timeout       time.Duration
		expectedError string
		setup         func(suite)
	}{
		{
			name: "ok: no wait",
			setup: func(s suite) {
				s.rpcClient.EXPECT().Status(mock.Anything).Return(&ctypes.ResultStatus{
					SyncInfo: ctypes.SyncInfo{LatestBlockHeight: targetBlockHeight},
				}, nil)
			},
		},
		{
			name:    "ok: wait 1 time",
			timeout: time.Second * 2, // must exceed the wait loop duration
			setup: func(s suite) {
				s.rpcClient.EXPECT().Status(mock.Anything).Return(&ctypes.ResultStatus{
					SyncInfo: ctypes.SyncInfo{LatestBlockHeight: targetBlockHeight - 1},
				}, nil).Once()
				s.rpcClient.EXPECT().Status(mock.Anything).Return(&ctypes.ResultStatus{
					SyncInfo: ctypes.SyncInfo{LatestBlockHeight: targetBlockHeight},
				}, nil).Once()
			},
		},
		{
			name:          "fail: wait expired",
			timeout:       time.Millisecond,
			expectedError: "timeout exceeded waiting for block: context deadline exceeded",
			setup: func(s suite) {
				s.rpcClient.EXPECT().Status(mock.Anything).Return(&ctypes.ResultStatus{
					SyncInfo: ctypes.SyncInfo{LatestBlockHeight: targetBlockHeight - 1},
				}, nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			c := newClient(t, tt.setup)
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()

			err := c.WaitForBlockHeight(ctx, targetBlockHeight)

			if tt.expectedError != "" {
				require.EqualError(err, tt.expectedError)
				return
			}
			require.NoError(err)
		})
	}
}

func TestClientWaitForTx(t *testing.T) {
	var (
		ctx          = context.Background()
		hash         = "abcd"
		hashBytes, _ = hex.DecodeString(hash)
		result       = &ctypes.ResultTx{
			Hash: hashBytes,
		}
	)
	tests := []struct {
		name           string
		hash           string
		expectedError  string
		expectedResult *ctypes.ResultTx
		setup          func(suite)
	}{
		{
			name:          "fail: hash not in hex format",
			hash:          "zzz",
			expectedError: "unable to decode tx hash 'zzz': encoding/hex: invalid byte: U+007A 'z'",
		},
		{
			name:           "ok: tx found immediately",
			hash:           hash,
			expectedResult: result,
			setup: func(s suite) {
				s.rpcClient.EXPECT().Tx(ctx, hashBytes, false).Return(result, nil)
			},
		},
		{
			name:          "fail: tx returns an unexpected error",
			hash:          hash,
			expectedError: "fetching tx 'abcd': error while requesting node 'http://localhost:26657': oups",
			setup: func(s suite) {
				s.rpcClient.EXPECT().Tx(ctx, hashBytes, false).Return(nil, errors.New("oups"))
			},
		},
		{
			name:           "ok: tx found after 1 block",
			hash:           hash,
			expectedResult: result,
			setup: func(s suite) {
				// tx is not found
				s.rpcClient.EXPECT().Tx(ctx, hashBytes, false).Return(nil, errors.New("tx abcd not found")).Once()
				// wait for next block
				s.rpcClient.EXPECT().Status(ctx).Return(&ctypes.ResultStatus{
					SyncInfo: ctypes.SyncInfo{LatestBlockHeight: 1},
				}, nil).Once()
				s.rpcClient.EXPECT().Status(ctx).Return(&ctypes.ResultStatus{
					SyncInfo: ctypes.SyncInfo{LatestBlockHeight: 2},
				}, nil).Once()
				// next block reached, check tx again, this time it's found.
				s.rpcClient.EXPECT().Tx(ctx, hashBytes, false).Return(result, nil).Once()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			c := newClient(t, tt.setup)

			res, err := c.WaitForTx(ctx, tt.hash)

			if tt.expectedError != "" {
				require.EqualError(err, tt.expectedError)
				return
			}
			require.NoError(err)
			assert.Equal(tt.expectedResult, res)
		})
	}
}

func TestClientAccount(t *testing.T) {
	var (
		accountName = "bob"
		passphrase  = "passphrase"
	)
	r, err := cosmosaccount.NewInMemory()
	require.NoError(t, err)
	expectedAccount, _, err := r.Create(accountName)
	require.NoError(t, err)
	expectedAddr, err := expectedAccount.Address("cosmos")
	require.NoError(t, err)
	// Export created account to we can import it in the Client below.
	key, err := r.Export(accountName, passphrase)
	require.NoError(t, err)

	tests := []struct {
		name          string
		addressOrName string
		expectedError string
	}{
		{
			name:          "ok: find by name",
			addressOrName: expectedAccount.Name,
		},
		{
			name:          "ok: find by address",
			addressOrName: expectedAddr,
		},
		{
			name:          "fail: name not found",
			addressOrName: "unknown",
			expectedError: "decoding bech32 failed: invalid bech32 string length 7",
		},
		{
			name:          "fail: address not found",
			addressOrName: "cosmos1cs4hpwrpna6ucsgsa78jfp403l7gdynukrxkrv",
			expectedError: `account "cosmos1cs4hpwrpna6ucsgsa78jfp403l7gdynukrxkrv" does not exist`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				require = require.New(t)
				assert  = assert.New(t)
				c       = newClient(t, nil)
			)
			_, err := c.AccountRegistry.Import(accountName, key, passphrase)
			require.NoError(err)

			account, err := c.Account(tt.addressOrName)

			if tt.expectedError != "" {
				require.EqualError(err, tt.expectedError)
				return
			}
			require.NoError(err)
			assert.Equal(expectedAccount, account)
		})
	}
}

func TestClientAddress(t *testing.T) {
	var (
		accountName = "bob"
		passphrase  = "passphrase"
	)
	r, err := cosmosaccount.NewInMemory()
	require.NoError(t, err)
	expectedAccount, _, err := r.Create(accountName)
	require.NoError(t, err)
	// Export created account to we can import it in the Client below.
	key, err := r.Export(accountName, passphrase)
	require.NoError(t, err)

	tests := []struct {
		name           string
		accountName    string
		opts           []cosmosclient.Option
		expectedError  string
		expectedPrefix string
	}{
		{
			name:           "ok: name exists",
			accountName:    expectedAccount.Name,
			expectedPrefix: "cosmos",
		},
		{
			name: "ok: name exists with different prefix",
			opts: []cosmosclient.Option{
				cosmosclient.WithAddressPrefix("test"),
			},
			accountName:    expectedAccount.Name,
			expectedPrefix: "test",
		},
		{
			name:          "fail: name not found",
			accountName:   "unknown",
			expectedError: `account "unknown" does not exist`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				require = require.New(t)
				assert  = assert.New(t)
				c       = newClient(t, nil, tt.opts...)
			)
			_, err := c.AccountRegistry.Import(accountName, key, passphrase)
			require.NoError(err)

			address, err := c.Address(tt.accountName)

			if tt.expectedError != "" {
				require.EqualError(err, tt.expectedError)
				return
			}
			require.NoError(err)
			expectedAddr, err := expectedAccount.Address(tt.expectedPrefix)
			require.NoError(err)
			assert.Equal(expectedAddr, address)
		})
	}
}

func TestClientStatus(t *testing.T) {
	var (
		ctx            = context.Background()
		expectedStatus = &ctypes.ResultStatus{
			NodeInfo: p2p.DefaultNodeInfo{Network: "mychain"},
		}
	)
	tests := []struct {
		name          string
		expectedError string
		setup         func(suite)
	}{
		{
			name: "ok",
			setup: func(s suite) {
				s.rpcClient.EXPECT().Status(ctx).Return(expectedStatus, nil).Once()
			},
		},
		{
			name:          "fail",
			expectedError: "error while requesting node 'http://localhost:26657': oups",
			setup: func(s suite) {
				s.rpcClient.EXPECT().Status(ctx).Return(expectedStatus, errors.New("oups")).Once()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClient(t, tt.setup)

			status, err := c.Status(ctx)

			if tt.expectedError != "" {
				require.EqualError(t, err, tt.expectedError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, expectedStatus, status)
		})
	}
}

func TestClientCreateTx(t *testing.T) {
	var (
		ctx         = context.Background()
		accountName = "bob"
		passphrase  = "passphrase"
	)
	r, err := cosmosaccount.NewInMemory()
	require.NoError(t, err)
	a, _, err := r.Create(accountName)
	require.NoError(t, err)
	// Export created account to we can import it in the Client below.
	key, err := r.Export(accountName, passphrase)
	require.NoError(t, err)
	sdkaddress, err := a.Record.GetAddress()
	require.NoError(t, err)

	tests := []struct {
		name           string
		opts           []cosmosclient.Option
		msg            sdktypes.Msg
		expectedJSONTx string
		expectedError  string
		setup          func(s suite)
	}{
		{
			name:          "fail: account doesn't exist",
			expectedError: "nope",
			setup: func(s suite) {
				s.accountRetriever.EXPECT().
					EnsureExists(mock.Anything, sdkaddress).Return(errors.New("nope"))
			},
		},
		{
			name: "ok: with default values",
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[],"gas_limit":"300000","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectPrepareFactory(sdkaddress)
			},
		},
		{
			name: "ok: with faucet enabled, account balance is high enough",
			opts: []cosmosclient.Option{
				cosmosclient.WithUseFaucet("localhost:1234", "", 0),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[],"gas_limit":"300000","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectMakeSureAccountHasToken(sdkaddress.String(), defaultFaucetMinAmount)

				s.expectPrepareFactory(sdkaddress)
			},
		},
		{
			name: "ok: with faucet enabled, account balance is too low",
			opts: []cosmosclient.Option{
				cosmosclient.WithUseFaucet("localhost:1234", "", 0),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[],"gas_limit":"300000","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectMakeSureAccountHasToken(sdkaddress.String(), defaultFaucetMinAmount-1)
				s.expectPrepareFactory(sdkaddress)
			},
		},
		{
			name: "ok: with fees",
			opts: []cosmosclient.Option{
				cosmosclient.WithFees("10token"),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[{"denom":"token","amount":"10"}],"gas_limit":"300000","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectPrepareFactory(sdkaddress)
			},
		},
		{
			name: "ok: with gas price",
			opts: []cosmosclient.Option{
				// Should set fees to 3*defaultGasLimit
				cosmosclient.WithGasPrices("3token"),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[{"denom":"token","amount":"900000"}],"gas_limit":"300000","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectPrepareFactory(sdkaddress)
			},
		},
		{
			name: "fail: with fees and gas prices",
			opts: []cosmosclient.Option{
				cosmosclient.WithFees("10token"),
				cosmosclient.WithGasPrices("3token"),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedError: "cannot provide both fees and gas prices",
			setup: func(s suite) {
				s.expectPrepareFactory(sdkaddress)
			},
		},
		{
			name: "ok: without empty gas limit",
			opts: []cosmosclient.Option{
				cosmosclient.WithGas(""),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[],"gas_limit":"20042","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectPrepareFactory(sdkaddress)
				s.gasometer.EXPECT().
					CalculateGas(mock.Anything, mock.Anything, mock.Anything).
					Return(nil, 42, nil)
			},
		},
		{
			name: "ok: without auto gas limit",
			opts: []cosmosclient.Option{
				cosmosclient.WithGas("auto"),
			},
			msg: &banktypes.MsgSend{
				FromAddress: "from",
				ToAddress:   "to",
				Amount: sdktypes.NewCoins(
					sdktypes.NewCoin("token", sdktypes.NewIntFromUint64((1))),
				),
			},
			expectedJSONTx: `{"body":{"messages":[{"@type":"/cosmos.bank.v1beta1.MsgSend","from_address":"from","to_address":"to","amount":[{"denom":"token","amount":"1"}]}],"memo":"","timeout_height":"0","extension_options":[],"non_critical_extension_options":[]},"auth_info":{"signer_infos":[],"fee":{"amount":[],"gas_limit":"20042","payer":"","granter":""},"tip":null},"signatures":[]}`,
			setup: func(s suite) {
				s.expectPrepareFactory(sdkaddress)
				s.gasometer.EXPECT().
					CalculateGas(mock.Anything, mock.Anything, mock.Anything).
					Return(nil, 42, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				require = require.New(t)
				assert  = assert.New(t)
				c       = newClient(t, tt.setup, tt.opts...)
			)
			account, err := c.AccountRegistry.Import(accountName, key, passphrase)
			require.NoError(err)

			txs, err := c.CreateTx(ctx, account, tt.msg)

			if tt.expectedError != "" {
				require.EqualError(err, tt.expectedError)
				return
			}
			require.NoError(err)
			assert.NotNil(txs)
			bz, err := txs.EncodeJSON()
			require.NoError(err)
			assert.JSONEq(tt.expectedJSONTx, string(bz))
		})
	}
}

func (s suite) expectMakeSureAccountHasToken(address string, balance int64) {
	currentBalance := sdktypes.NewInt64Coin(defaultFaucetDenom, balance)
	s.bankQueryClient.EXPECT().Balance(
		context.Background(),
		&banktypes.QueryBalanceRequest{
			Address: address,
			Denom:   defaultFaucetDenom,
		},
	).Return(
		&banktypes.QueryBalanceResponse{
			Balance: &currentBalance,
		},
		nil,
	).Once()
	if balance >= defaultFaucetMinAmount {
		// balance is high enought, faucet won't be called
		return
	}

	s.faucetClient.EXPECT().Transfer(context.Background(),
		cosmosfaucet.TransferRequest{AccountAddress: address},
	).Return(
		cosmosfaucet.TransferResponse{}, nil,
	)

	newBalance := sdktypes.NewInt64Coin(defaultFaucetDenom, defaultFaucetMinAmount)
	s.bankQueryClient.EXPECT().Balance(
		mock.Anything,
		&banktypes.QueryBalanceRequest{
			Address: address,
			Denom:   defaultFaucetDenom,
		},
	).Return(
		&banktypes.QueryBalanceResponse{
			Balance: &newBalance,
		},
		nil,
	).Once()
}

func (s suite) expectPrepareFactory(sdkaddress sdktypes.Address) {
	s.accountRetriever.EXPECT().
		EnsureExists(mock.Anything, sdkaddress).
		Return(nil)
	s.accountRetriever.EXPECT().
		GetAccountNumberSequence(mock.Anything, sdkaddress).
		Return(1, 2, nil)
}

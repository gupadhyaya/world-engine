package keeper_test

import (
	"cosmossdk.io/core/header"
	storetypes "cosmossdk.io/store/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/testutil"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	"github.com/stretchr/testify/suite"
	"pkg.world.dev/world-engine/evm/x/namespace"
	"pkg.world.dev/world-engine/evm/x/namespace/keeper"
	namespacetypes "pkg.world.dev/world-engine/evm/x/namespace/types"
	"testing"
	"time"
)

type TestSuite struct {
	suite.Suite

	ctx         sdk.Context
	addrs       []sdk.AccAddress
	authority   sdk.AccAddress
	queryClient namespacetypes.QueryServiceClient
	keeper      *keeper.Keeper

	encCfg moduletestutil.TestEncodingConfig
}

func TestRunSuite(t *testing.T) {
	suite.Run(t, new(TestSuite))
}

func (s *TestSuite) SetupTest() {
	sdk.GetConfig().SetBech32PrefixForAccount("world", "world")
	// suite setup
	s.addrs = simtestutil.CreateIncrementalAccounts(3)
	s.authority = s.addrs[0]
	s.encCfg = moduletestutil.MakeTestEncodingConfig(namespace.AppModuleBasic{})
	key := storetypes.NewKVStoreKey(namespacetypes.ModuleName)
	testCtx := testutil.DefaultContextWithDB(s.T(), key, storetypes.NewTransientStoreKey("transient_test"))
	s.ctx = testCtx.Ctx.WithHeaderInfo(header.Info{Time: time.Now().Round(0).UTC()})

	s.keeper = keeper.NewKeeper(key, s.authority.String())

	queryHelper := baseapp.NewQueryServerTestHelper(s.ctx, s.encCfg.InterfaceRegistry)
	namespacetypes.RegisterQueryServiceServer(queryHelper, s.keeper)

	s.queryClient = namespacetypes.NewQueryServiceClient(queryHelper)
}

func (s *TestSuite) TestGetAndSetNamespace() {
	ns := &namespacetypes.Namespace{
		ShardName:    "foobar",
		ShardAddress: "localhost:9310",
	}
	_, err := s.keeper.UpdateNamespace(s.ctx, &namespacetypes.UpdateNamespaceRequest{
		Authority: s.authority.String(),
		Namespace: ns,
	})
	s.Require().NoError(err)

	// happy path
	res, err := s.keeper.Address(s.ctx, &namespacetypes.AddressRequest{Namespace: ns.ShardName})
	s.Require().NoError(err)
	s.Require().Equal(res.Address, ns.ShardAddress)

	// no bueno path
	notExistsNs := "hello_world"
	_, err = s.keeper.Address(s.ctx, &namespacetypes.AddressRequest{Namespace: notExistsNs})
	s.Require().EqualError(err, "address for namespace "+notExistsNs+" does not exist")
}

func (s *TestSuite) TestGetAllNamespaces() {
	namespaces := map[string]*namespacetypes.Namespace{
		"foo": {
			ShardName:    "foo",
			ShardAddress: "bar",
		},
		"bar": {
			ShardName:    "bar",
			ShardAddress: "foo",
		},
		"baz": {
			ShardName:    "baz",
			ShardAddress: "qux",
		},
	}
	for _, ns := range namespaces {
		_, err := s.keeper.UpdateNamespace(s.ctx, &namespacetypes.UpdateNamespaceRequest{
			Authority: s.authority.String(),
			Namespace: ns,
		})
		s.Require().NoError(err)
	}

	res, err := s.keeper.Namespaces(s.ctx, &namespacetypes.NamespacesRequest{})
	s.Require().NoError(err)
	s.Require().Equal(len(res.Namespaces), len(namespaces))

	for _, gotNs := range res.Namespaces {
		ns, ok := namespaces[gotNs.ShardName]
		s.Require().True(ok, "no matching namespace found for %s", gotNs.ShardName)
		s.Require().Equal(ns, gotNs)
	}
}

func (s *TestSuite) TestUpdateNamespace_Unauthorized() {
	notAuth := s.addrs[1].String()
	_, err := s.keeper.UpdateNamespace(s.ctx, &namespacetypes.UpdateNamespaceRequest{
		Authority: notAuth,
		Namespace: nil,
	})
	s.Require().ErrorContains(err, notAuth+" is not allowed to update namespaces")
}

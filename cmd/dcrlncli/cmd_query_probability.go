package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/routing/route"
	"github.com/urfave/cli"
)

var queryProbCommand = cli.Command{
	Name:      "queryprob",
	Category:  "Payments",
	Usage:     "Estimate a success probability.",
	ArgsUsage: "from-node to-node amt",
	Action:    actionDecorator(queryProb),
}

func queryProb(ctx *cli.Context) error {
	args := ctx.Args()

	if len(args) != 3 {
		return cli.ShowCommandHelp(ctx, "queryprob")
	}

	fromNode, err := route.NewVertexFromStr(args.Get(0))
	if err != nil {
		return fmt.Errorf("invalid from node key: %v", err)
	}

	toNode, err := route.NewVertexFromStr(args.Get(1))
	if err != nil {
		return fmt.Errorf("invalid to node key: %v", err)
	}

	amtSat, err := strconv.ParseUint(args.Get(2), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid amt: %v", err)
	}

	amtMAtoms := lnwire.NewMAtomsFromAtoms(
		dcrutil.Amount(amtSat),
	)

	conn := getClientConn(ctx, false)
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	req := &routerrpc.QueryProbabilityRequest{
		FromNode:  fromNode[:],
		ToNode:    toNode[:],
		AmtMAtoms: int64(amtMAtoms),
	}
	rpcCtx := context.Background()
	response, err := client.QueryProbability(rpcCtx, req)
	if err != nil {
		return err
	}

	printRespJSON(response)

	return nil
}

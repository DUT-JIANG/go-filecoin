package commands

import (
	"encoding/json"
	"io"

	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	cmdkit "github.com/ipfs/go-ipfs-cmdkit"
	cmds "github.com/ipfs/go-ipfs-cmds"

	"github.com/filecoin-project/go-filecoin/internal/pkg/types"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/actor"
)

// ActorView represents a generic way to represent details about any actor to the user.
type ActorView struct {
	Address string        `json:"address"`
	Code    cid.Cid       `json:"code,omitempty"`
	Nonce   uint64        `json:"nonce"`
	Balance types.AttoFIL `json:"balance"`
	Head    cid.Cid       `json:"head,omitempty"`
}

var actorCmd = &cmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Interact with actors. Actors are built-in smart contracts.",
	},
	Subcommands: map[string]*cmds.Command{
		"ls": actorLsCmd,
	},
}

var actorLsCmd = &cmds.Command{
	Run: func(req *cmds.Request, re cmds.ResponseEmitter, env cmds.Environment) error {
		results, err := GetPorcelainAPI(env).ActorLs(req.Context)
		if err != nil {
			return err
		}

		for result := range results {
			if result.Error != nil {
				return result.Error
			}

			output := makeActorView(result.Actor, result.Key)
			if err := re.Emit(output); err != nil {
				return err
			}
		}
		return nil
	},
	Type: &ActorView{},
	Encoders: cmds.EncoderMap{
		cmds.JSON: cmds.MakeTypedEncoder(func(req *cmds.Request, w io.Writer, a *ActorView) error {
			marshaled, err := json.Marshal(a)
			if err != nil {
				return err
			}
			_, err = w.Write(marshaled)
			if err != nil {
				return err
			}
			_, err = w.Write([]byte("\n"))
			return err
		}),
	},
}

func makeActorView(act *actor.Actor, addr address.Address) *ActorView {
	return &ActorView{
		Address: addr.String(),
		Code:    act.Code.Cid,
		Nonce:   act.CallSeqNum,
		Balance: types.NewAttoFILFromFIL(uint64(act.Balance.Int64())),
		Head:    act.Head.Cid,
	}
}

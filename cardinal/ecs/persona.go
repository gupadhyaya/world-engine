package ecs

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rotisserie/eris"
	"pkg.world.dev/world-engine/cardinal/types/component"
	"pkg.world.dev/world-engine/cardinal/types/entity"
)

// CreatePersona allows for the associating of a persona tag with a signer address.
type CreatePersona struct {
	PersonaTag    string `json:"personaTag"`
	SignerAddress string `json:"signerAddress"`
}

type CreatePersonaResult struct {
	Success bool `json:"success"`
}

// CreatePersonaMsg is a message that facilitates the creation of a persona tag.
var CreatePersonaMsg = NewMessageType[CreatePersona, CreatePersonaResult](
	"create-persona",
	WithMsgEVMSupport[CreatePersona, CreatePersonaResult],
)

var regexpObj = regexp.MustCompile("^[a-zA-Z0-9_]+$")

type AuthorizePersonaAddress struct {
	Address string `json:"address"`
}

type AuthorizePersonaAddressResult struct {
	Success bool `json:"success"`
}

var AuthorizePersonaAddressMsg = NewMessageType[AuthorizePersonaAddress, AuthorizePersonaAddressResult](
	"authorize-persona-address",
)

// AuthorizePersonaAddressSystem enables users to authorize an address to a persona tag. This is mostly used so that
// users who want to interact with the game via smart contract can link their EVM address to their persona tag, enabling
// them to mutate their owned state from the context of the EVM.
func AuthorizePersonaAddressSystem(wCtx WorldContext) error {
	personaTagToAddress, err := buildPersonaTagMapping(wCtx)
	if err != nil {
		return err
	}

	AuthorizePersonaAddressMsg.Each(
		wCtx, func(txData TxData[AuthorizePersonaAddress]) (result AuthorizePersonaAddressResult, err error) {
			msg, tx := txData.Msg, txData.Tx
			result.Success = false

			// Check if the Persona Tag exists
			lowerPersona := strings.ToLower(tx.PersonaTag)
			data, ok := personaTagToAddress[lowerPersona]
			if !ok {
				return result, eris.Errorf("persona %s does not exist", tx.PersonaTag)
			}

			// Check that the ETH Address is valid
			msg.Address = strings.ToLower(msg.Address)
			msg.Address = strings.ReplaceAll(msg.Address, " ", "")
			valid := common.IsHexAddress(msg.Address)
			if !valid {
				return result, eris.Errorf("eth address %s is invalid", msg.Address)
			}

			err = updateComponent[SignerComponent](
				wCtx, data.EntityID, func(s *SignerComponent) *SignerComponent {
					for _, addr := range s.AuthorizedAddresses {
						if addr == msg.Address {
							return s
						}
					}
					s.AuthorizedAddresses = append(s.AuthorizedAddresses, msg.Address)
					return s
				},
			)
			if err != nil {
				return result, eris.Wrap(err, "unable to update signer component with address")
			}
			result.Success = true
			return result, nil
		},
	)
	return nil
}

type SignerComponent struct {
	PersonaTag          string
	SignerAddress       string
	AuthorizedAddresses []string
}

func (SignerComponent) Name() string {
	return "SignerComponent"
}

type personaTagComponentData struct {
	SignerAddress string
	EntityID      entity.ID
}

func buildPersonaTagMapping(wCtx WorldContext) (map[string]personaTagComponentData, error) {
	personaTagToAddress := map[string]personaTagComponentData{}
	var errs []error
	q, err := wCtx.NewSearch(Exact(SignerComponent{}))
	if err != nil {
		return nil, err
	}
	err = q.Each(
		wCtx, func(id entity.ID) bool {
			sc, err := getComponent[SignerComponent](wCtx, id)
			if err != nil {
				errs = append(errs, err)
				return true
			}
			lowerPersona := strings.ToLower(sc.PersonaTag)
			personaTagToAddress[lowerPersona] = personaTagComponentData{
				SignerAddress: sc.SignerAddress,
				EntityID:      id,
			}
			return true
		},
	)
	if err != nil {
		return nil, err
	}
	if len(errs) != 0 {
		return nil, errors.Join(errs...)
	}
	return personaTagToAddress, nil
}

// RegisterPersonaSystem is an ecs.System that will associate persona tags with signature addresses. Each persona tag
// may have at most 1 signer, so additional attempts to register a signer with a persona tag will be ignored.
func RegisterPersonaSystem(wCtx WorldContext) error {
	personaTagToAddress, err := buildPersonaTagMapping(wCtx)
	if err != nil {
		return err
	}

	CreatePersonaMsg.Each(wCtx, func(txData TxData[CreatePersona]) (result CreatePersonaResult, err error) {
		msg := txData.Msg
		result.Success = false

		if !isAlphanumericWithUnderscore(msg.PersonaTag) {
			err = eris.Errorf("persona tag %s is not valid: must only contain alphanumerics and underscores", msg.PersonaTag)
			return result, err
		}

		// Temporarily convert tag to lowercase to check against mapping of lowercase tags
		lowerPersona := strings.ToLower(msg.PersonaTag)
		if _, ok := personaTagToAddress[lowerPersona]; ok {
			// This PersonaTag has already been registered. Don't do anything
			err = eris.Errorf("persona tag %s has already been registered", msg.PersonaTag)
			return result, err
		}
		id, err := create(wCtx, SignerComponent{})
		if err != nil {
			return result, eris.Wrap(err, "")
		}
		if err = setComponent[SignerComponent](
			wCtx, id, &SignerComponent{
				PersonaTag:    msg.PersonaTag,
				SignerAddress: msg.SignerAddress,
			},
		); err != nil {
			return result, eris.Wrap(err, "")
		}
		personaTagToAddress[lowerPersona] = personaTagComponentData{
			SignerAddress: msg.SignerAddress,
			EntityID:      id,
		}
		result.Success = true
		return result, nil
	})

	return nil
}

func isAlphanumericWithUnderscore(s string) bool {
	// Use the MatchString method to check if the string matches the pattern
	return regexpObj.MatchString(s)
}

var (
	ErrPersonaTagHasNoSigner        = errors.New("persona tag does not have a signer")
	ErrCreatePersonaTxsNotProcessed = errors.New("create persona txs have not been processed for the given tick")
)

// GetSignerForPersonaTag returns the signer address that has been registered for the given persona tag after the
// given tick. If the world's tick is less than or equal to the given tick, ErrorCreatePersonaTXsNotProcessed is
// returned. If the given personaTag has no signer address, ErrPersonaTagHasNoSigner is returned.
func (w *World) GetSignerForPersonaTag(personaTag string, tick uint64) (addr string, err error) {
	if tick >= w.CurrentTick() {
		return "", ErrCreatePersonaTxsNotProcessed
	}
	var errs []error
	q, err := w.NewSearch(Exact(SignerComponent{}))
	if err != nil {
		return "", err
	}
	wCtx := NewReadOnlyWorldContext(w)
	err = q.Each(
		wCtx, func(id entity.ID) bool {
			sc, err := getComponent[SignerComponent](wCtx, id)
			if err != nil {
				errs = append(errs, err)
			}
			if sc.PersonaTag == personaTag {
				addr = sc.SignerAddress
				return false
			}
			return true
		},
	)
	errs = append(errs, err)
	if addr == "" {
		return "", ErrPersonaTagHasNoSigner
	}
	return addr, errors.Join(errs...)
}

// TODO private component function used to temporarily remove circular dependency until we replace components.
// TODO this function is intended only for use with persona.go and is to be removed with persona when we replace with
// plugins.
// Get returns component data from the entity.
// GetComponent returns component data from the entity.
func getComponent[T component.Component](wCtx WorldContext, id entity.ID) (comp *T, err error) {
	var t T
	name := t.Name()
	c, err := wCtx.GetWorld().GetComponentByName(name)
	if err != nil {
		return nil, eris.Wrap(err, "must register component")
	}
	value, err := wCtx.StoreReader().GetComponentForEntity(c, id)
	if err != nil {
		return nil, err
	}
	t, ok := value.(T)
	if !ok {
		comp, ok = value.(*T)
		if !ok {
			return nil, eris.Errorf("type assertion for component failed: %v to %v", value, c)
		}
	} else {
		comp = &t
	}

	return comp, nil
}

// setComponent sets component data to the entity.
//
// TODO private component function used to temporarily remove circular dependency until we replace components.
// TODO this function is intended only for use with persona.go and is to be removed with persona when we replace with
// plugins.
func setComponent[T component.Component](wCtx WorldContext, id entity.ID, component *T) error {
	if wCtx.IsReadOnly() {
		return eris.Wrap(ErrCannotModifyStateWithReadOnlyContext, "")
	}
	var t T
	name := t.Name()
	c, err := wCtx.GetWorld().GetComponentByName(name)
	if err != nil {
		return eris.Wrapf(err, "%s is not registered, please register it before updating", t.Name())
	}
	err = wCtx.StoreManager().SetComponentForEntity(c, id, component)
	if err != nil {
		return err
	}
	wCtx.Logger().Debug().
		Str("entity_id", strconv.FormatUint(uint64(id), 10)).
		Str("component_name", c.Name()).
		Int("component_id", int(c.ID())).
		Msg("entity updated")
	return nil
}

// TODO private component function used to temporarily remove circular dependency until we replace components.
// TODO this function is intended only for use with persona.go and is to be removed with persona when we replace with
// plugins.
// https://linear.app/arguslabs/issue/WORLD-423/ecs-plugin-feature
func updateComponent[T component.Component](wCtx WorldContext, id entity.ID, fn func(*T) *T) error {
	if wCtx.IsReadOnly() {
		return eris.Wrap(ErrCannotModifyStateWithReadOnlyContext, "")
	}
	val, err := getComponent[T](wCtx, id)
	if err != nil {
		return err
	}
	updatedVal := fn(val)
	return setComponent[T](wCtx, id, updatedVal)
}

// TODO private component function used to temporarily remove circular dependency until we replace components.
// TODO this function is intended only for use with persona.go and is to be removed with persona when we replace with
// plugins.
// https://linear.app/arguslabs/issue/WORLD-423/ecs-plugin-feature
func createMany(wCtx WorldContext, num int, components ...component.Component) ([]entity.ID, error) {
	if wCtx.IsReadOnly() {
		return nil, eris.Wrap(ErrCannotModifyStateWithReadOnlyContext, "")
	}
	world := wCtx.GetWorld()
	acc := make([]component.ComponentMetadata, 0, len(components))
	for _, comp := range components {
		c, err := world.GetComponentByName(comp.Name())
		if err != nil {
			return nil, err
		}
		acc = append(acc, c)
	}
	entityIds, err := world.StoreManager().CreateManyEntities(num, acc...)
	if err != nil {
		return nil, err
	}
	for _, id := range entityIds {
		for _, comp := range components {
			c, err := world.GetComponentByName(comp.Name())
			if err != nil {
				return nil, eris.Wrap(err, "must register component before creating an entity")
			}
			err = world.StoreManager().SetComponentForEntity(c, id, comp)
			if err != nil {
				return nil, err
			}
		}
	}
	return entityIds, nil
}

// TODO private component function used to temporarily remove circular dependency until we replace components.
// TODO this function is intended only for use with persona.go and is to be removed with persona when we replace with
// plugins.
// https://linear.app/arguslabs/issue/WORLD-423/ecs-plugin-feature
func create(wCtx WorldContext, components ...component.Component) (entity.ID, error) {
	entities, err := createMany(wCtx, 1, components...)
	if err != nil {
		return 0, err
	}
	return entities[0], nil
}

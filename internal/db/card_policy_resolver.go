package db

import "github.com/1239t/vohive/internal/cardpolicy"

// CardPolicyResolver 用 DB 实现 cardpolicy.Resolver。
type CardPolicyResolver struct{}

func (CardPolicyResolver) Resolve(iccid string) (cardpolicy.Policy, error) {
	p, err := ResolveCardPolicy(iccid)
	if err != nil {
		return cardpolicy.Policy{}, err
	}
	return cardpolicy.Policy{
		ICCID:           p.ICCID,
		NetworkEnabled:  p.NetworkEnabled,
		VoWiFiEnabled:   p.VoWiFiEnabled,
		AirplaneEnabled: p.AirplaneEnabled,
		IPVersion:       p.IPVersion,
		APN:             p.APN,
	}, nil
}

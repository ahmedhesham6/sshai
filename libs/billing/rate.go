package billing

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"
)

type ResourceType string

const (
	ResourceCompute ResourceType = "compute"
	ResourceStorage ResourceType = "storage"
)

var decimalPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

type CreditRate struct {
	version        string
	resourceType   ResourceType
	region         string
	preset         string
	rawUnit        string
	creditsPerUnit *big.Rat
	creditsText    string
	effectiveAt    time.Time
}

func NewCreditRate(
	version string,
	resourceType ResourceType,
	region string,
	preset string,
	rawUnit string,
	creditsPerUnit string,
	effectiveAt time.Time,
) (CreditRate, error) {
	if version == "" || region == "" || rawUnit == "" || effectiveAt.IsZero() {
		return CreditRate{}, errors.New("rate version, region, raw unit, and effective time are required")
	}
	switch resourceType {
	case ResourceCompute:
		if preset == "" {
			return CreditRate{}, errors.New("compute rate requires a runtime preset")
		}
	case ResourceStorage:
		if preset != "" {
			return CreditRate{}, errors.New("storage rate cannot have a runtime preset")
		}
	default:
		return CreditRate{}, errors.New("unsupported resource type")
	}
	rate, err := parseDecimal(creditsPerUnit)
	if err != nil || rate.Sign() <= 0 {
		return CreditRate{}, errors.New("credits per unit must be a positive decimal")
	}
	return CreditRate{
		version:        version,
		resourceType:   resourceType,
		region:         region,
		preset:         preset,
		rawUnit:        rawUnit,
		creditsPerUnit: rate,
		creditsText:    creditsPerUnit,
		effectiveAt:    effectiveAt.Round(0).UTC(),
	}, nil
}

func (r CreditRate) Version() string { return r.version }

func (r CreditRate) ResourceType() ResourceType { return r.resourceType }

func (r CreditRate) Region() string { return r.region }

func (r CreditRate) Preset() string { return r.preset }

func (r CreditRate) RawUnit() string { return r.rawUnit }

func (r CreditRate) CreditsPerUnit() string { return r.creditsText }

func (r CreditRate) EffectiveAt() time.Time { return r.effectiveAt }

func (r CreditRate) Convert(rawQuantity string) (int64, error) {
	if r.creditsPerUnit == nil {
		return 0, errors.New("credit rate is not initialized")
	}
	quantity, err := parseDecimal(rawQuantity)
	if err != nil {
		return 0, fmt.Errorf("raw quantity: %w", err)
	}
	credits := new(big.Rat).Mul(quantity, r.creditsPerUnit)
	if !credits.IsInt() || !credits.Num().IsInt64() {
		return 0, errors.New("conversion must produce a whole int64 credit amount")
	}
	return credits.Num().Int64(), nil
}

func parseDecimal(value string) (*big.Rat, error) {
	if !decimalPattern.MatchString(value) {
		return nil, errors.New("must be an unsigned base-10 decimal")
	}
	decimal, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, errors.New("invalid decimal")
	}
	return decimal, nil
}

package dal

import "github.com/google/wire"

var ProviderSet = wire.NewSet(NewProfileDal, NewGroupDal, NewSubscriptionDal)

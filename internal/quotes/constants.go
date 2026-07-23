package quotes

import "time"

// ---- domain constants ----

// quoteTTL is how long a quote stays valid (SPEC: 3 minutes).
const quoteTTL = 3 * time.Minute

// ---- log messages ----

const logMsgIncrementDemandFailed = "quotes: increment demand failed"

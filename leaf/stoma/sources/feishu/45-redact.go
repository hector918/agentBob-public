package feishu

import "agentbob/leaf/stoma/sources/common"

// redactErr scrubs secrets from an error before it is logged. It delegates to
// common.RedactErr so feishu gets the SAME treatment as discord/email: the app
// secret is replaced AND the generic scrubber runs over the rest (DSN passwords,
// URL tokens, …), instead of only hand-stripping the app secret (S-34). A nil
// error or one carrying no secret is returned unchanged (type identity kept).
func (s *Source) redactErr(err error) error {
	return common.RedactErr(err, s.appSecret)
}

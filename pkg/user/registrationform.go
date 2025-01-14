package user

import (
	"fmt"
	"html"
	"net/http"
	"net/url"

	"github.com/budden/semdict/pkg/sddb"

	"github.com/budden/semdict/pkg/apperror"
	"github.com/budden/semdict/pkg/shared"
	"github.com/gin-gonic/gin"
)

// RegistrationFormPageHandler renders a /registrationform page
func RegistrationFormPageHandler(c *gin.Context) {
	EnsureNotLoggedIn(c)
	c.HTML(http.StatusOK,
		"registrationform.t.html",
		shared.GeneralTemplateParams{Message: "Search Form"})
}

// RegistrationSubmitPostHandler processes a registrationsubmit form post request
func RegistrationSubmitPostHandler(c *gin.Context) {
	EnsureNotLoggedIn(c)
	var rd RegistrationData
	rd.Nickname = c.PostForm("nickname")
	rd.Registrationemail = c.PostForm("registrationemail")
	rd.Password1 = c.PostForm("password1")
	rd.Password2 = c.PostForm("password2")
	appErr := doRegistrationSubmit(c, &rd)
	if appErr == nil {
		c.HTML(http.StatusOK,
			"general.t.html",
			shared.GeneralTemplateParams{
				Message: "Check your E-Mail for a confirmation code, which will be valid for 10 minutes"})
	} else {
		c.HTML(http.StatusOK,
			"general.t.html",
			shared.GeneralTemplateParams{Message: appErr.Message})
	}
}

func doRegistrationSubmit(c *gin.Context, rd *RegistrationData) (apperr *apperror.AppErr) {
	validateRegistrationData(rd)
	apperr = processRegistrationSubmitWithDb(rd)
	if apperr == nil {
		// sendConfirmationEmail only produces 500 in case of failure
		sendConfirmationEmail(c, rd)
	}
	return apperr
}

func validateRegistrationData(rd *RegistrationData) {
	if !isNicknameInValidFormat(rd.Nickname) {
		apperror.Panic500If(apperror.ErrDummy, "Nickname is invalid")
	}
	if rd.Password1 != rd.Password2 {
		apperror.Panic500If(apperror.ErrDummy, "Passwords don't match")
	}
	passwordErr := validatePassword(rd.Password1)
	if passwordErr != nil {
		apperror.Panic500If(apperror.ErrDummy, "%s", passwordErr.Error())
	}
	if !isEmailInValidFormat(rd.Registrationemail) {
		apperror.Panic500If(apperror.ErrDummy, "Email is invalid")
	}
}

func sendConfirmationEmail(c *gin.Context, rd *RegistrationData) {
	scd := shared.SecretConfigData
	// TODO: if there are no certificate files, use http an7
	confirmationLinkBase := shared.SitesProtocol() + "//" + scd.SiteRoot + shared.SitesPort() + "/registrationconfirmation"
	parameters := url.Values{"nickname": {rd.Nickname}, "confirmationkey": {rd.ConfirmationKey}}
	u, err := url.Parse(confirmationLinkBase)
	apperror.GracefullyExitAppIf(err, "Unable to parse base URL for a confirmation link")
	u.RawQuery = parameters.Encode()
	confirmationLink := u.String()
	body := fmt.Sprintf(
		"Hello, %s!\nTo activate your account, please follow an activation link: <a href=%s>%s</a>",
		// FIXME should Nickname need html escaping?
		html.EscapeString(rd.Nickname),
		confirmationLink, confirmationLink)

	err = SendEmail(
		rd.Registrationemail,
		"Welcome to semantic dictionary!",
		body)

	if err != nil {
		// We assume that failure to send an E-mail can be due to temporary
		// network issues
		apperror.Panic500AndLogAttackIf(err, c, "Failed to send a confirmation E-mail")
	}

	noteRegistrationConfirmationEMailSentWithDb(rd)
	return
}

// rd.UserID is filled
func noteRegistrationConfirmationEMailSentWithDb(rd *RegistrationData) {
	err := sddb.WithTransaction(func(trans *sddb.TransactionType) (err1 error) {
		sddb.CheckDbAlive()
		_, err1 = trans.Tx.NamedExec(
			`select note_registrationconfirmation_email_sent(:nickname, :confirmationkey)`,
			rd)
		return
	})
	sddb.FatalDatabaseErrorIf(err, "Error remembering that E-Mail was sent, error is %#v", err)
	return
}

var mapViolatedConstraintNameToMessage = map[string]string{
	"i_registrationattempt__confirmationkey":   "You're lucky to hit a very seldom random number clash. Please retry a registration",
	"i_registrationattempt__registrationemail": "Someone is already trying to register with the same E-mail",
	"i_registrationattempt__nickname":          "Someone is already trying to register with the same Nickname",
	"i_sduser_registrationemail":               "There is already a user with the same E-mail",
	"i_sdusernickname":                         "There is already a user with the same nickname"}

func deleteExpiredRegistrationAttempts(trans *sddb.TransactionType) error {
	tx := trans.Tx
	sddb.CheckDbAlive()
	_, err1 := tx.Exec("select delete_expired_registrationattempts()")
	// it's not a fatal error (rare case!)
	apperror.Panic500If(err1,
		"Failed to register. Please try again later or contact us for assistance")
	sddb.CheckDbAlive()
	err1 = tx.Commit()
	sddb.FatalDatabaseErrorIf(err1,
		"Failed to commit after delete_expired_registrationattempts, error = %#v",
		err1)
	return nil
}

// processRegistrationSubmitWithDb inserts a registration attempt into sdusers_db
// If some "normal" error happens like non-unique nickname, it is returned in dberror.
func processRegistrationSubmitWithDb(rd *RegistrationData) *apperror.AppErr {

	err := sddb.WithTransaction(deleteExpiredRegistrationAttempts)
	sddb.FatalDatabaseErrorIf(err,
		"Failed around delete_expired_registrationattempts, %#v",
		err)

	err = sddb.WithTransaction(func(trans *sddb.TransactionType) (err error) {
		rd.Salt, rd.Hash = SaltAndHashPassword(rd.Password1)
		rd.ConfirmationKey = GenNonce(20)
		sddb.CheckDbAlive()
		_, err = trans.Tx.NamedExec(
			`select add_registrationattempt(:nickname, :salt, :hash, :registrationemail, :confirmationkey)`,
			rd)
		if err == nil {
			sddb.CheckDbAlive()
			err = trans.Tx.Commit()
		}
		return
	})
	return handleRegistrationAttemptInsertError(err)
}

func handleRegistrationAttemptInsertError(err error) *apperror.AppErr {
	//xt := reflect.TypeOf(err1).Kind()
	/* if e, ok := err.(*pgx.Error); ok {
		if e.Code == PostgresqlErrorCodeUniqueViolation {
			message, found := mapViolatedConstraintNameToMessage[e.Constraint]
			if found {
				return apperror.NewAppErrf(message)
			}
		}
	} */
	sddb.FatalDatabaseErrorIf(err, "Unexpected error in the registrationsubmit, %#v\n", err)
	return nil
}

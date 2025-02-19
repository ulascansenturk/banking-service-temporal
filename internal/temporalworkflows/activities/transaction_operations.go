package activities

import (
	"context"
	"fmt"
	"gorm.io/datatypes"
	"time"
	"ulascansenturk/service/internal/accounts"
	"ulascansenturk/service/internal/constants"
	"ulascansenturk/service/internal/helpers"
	"ulascansenturk/service/internal/transactions"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
)

type TransactionOperations struct {
	finderOrCreatorService transactions.FinderOrCreator
	transactionService     transactions.Service
	accountsService        accounts.Service
	timeProvider           helpers.TimeProvider
}

func NewTransactionOperations(finderOrCreatorService transactions.FinderOrCreator, transactionsService transactions.Service, accountsService accounts.Service, timeProvider helpers.TimeProvider) *TransactionOperations {
	return &TransactionOperations{
		finderOrCreatorService: finderOrCreatorService,
		transactionService:     transactionsService,
		accountsService:        accountsService,
		timeProvider:           timeProvider,
	}
}

type TransferParams struct {
	Amount                            int
	FeeAmount                         *int
	Metadata                          *map[string]interface{}
	DestinationAccountID              uuid.UUID
	SourceTransactionReferenceID      uuid.UUID
	DestinationTransactionReferenceID uuid.UUID
	FeeTransactionReferenceID         uuid.UUID
	SourceAccountID                   uuid.UUID
}

type TransferResult struct {
	SourceTransactionReferenceID      uuid.UUID
	DestinationTransactionReferenceID uuid.UUID
	FeeTransactionReferenceID         uuid.UUID
	FeeTransaction                    *transactions.Transaction
	SourceTransaction                 *transactions.Transaction
	DestinationTransaction            *transactions.Transaction
}

func (t *TransactionOperations) Transfer(ctx context.Context, params TransferParams) (*TransferResult, error) {
	validAccounts, accountsErr := t.validateAccount(ctx, params.Amount, params.FeeAmount, params.SourceAccountID, params.DestinationAccountID)
	if accountsErr != nil {
		return nil, temporal.NewNonRetryableApplicationError("Error on validating accounts", "validate-accounts-err", accountsErr)

	}

	pendingOutGoingTransaction, pendingOutGoingTransactionErr := t.createPendingOutgoingTransaction(ctx, params, *validAccounts.SourceAccount)
	if pendingOutGoingTransactionErr != nil {
		return nil, pendingOutGoingTransactionErr
	}

	pendingFeeTrx, pendingFeeTrxErr := t.createPendingFeeTransaction(ctx, params, *validAccounts.SourceAccount)
	if pendingFeeTrxErr != nil {
		return nil, pendingFeeTrxErr
	}

	pendingIncomingTransaction, pendingIncomingTransactionErr := t.createPendingIncomingTransaction(ctx, params, *validAccounts.DestinationAccount)
	if pendingIncomingTransactionErr != nil {
		return nil, pendingIncomingTransactionErr
	}

	if updateAccountBalanceErr := t.updateAccountBalances(ctx, *validAccounts.SourceAccount, *validAccounts.DestinationAccount, params); updateAccountBalanceErr != nil {
		return nil, updateAccountBalanceErr
	}

	updatedTransactions, finalizeTranscationErr := t.finalizeTransactions(ctx, pendingOutGoingTransaction, pendingIncomingTransaction, pendingFeeTrx)
	if finalizeTranscationErr != nil {
		return nil, finalizeTranscationErr
	}

	return t.createTransferResult(params, &updatedTransactions.OutgoingTrx, &updatedTransactions.IncomingTrx, updatedTransactions.FeeTrx), nil
}

func (t *TransactionOperations) createPendingOutgoingTransaction(ctx context.Context, params TransferParams, sourceAccount accounts.Account) (*transactions.Transaction, error) {
	pendingOutgoingTransactionParams := &transactions.Transaction{
		UserID:       &sourceAccount.UserID,
		Amount:       params.Amount,
		AccountID:    sourceAccount.ID,
		CurrencyCode: constants.CurrencyCode(sourceAccount.Currency),
		ReferenceID:  params.SourceTransactionReferenceID,
		Metadata: datatypes.JSONMap(map[string]interface{}{
			"OperationType":        "Transfer",
			"LinkedTransactionID":  params.SourceTransactionReferenceID.String(),
			"LinkedAccountID":      sourceAccount.ID.String(),
			"DestinationAccountID": params.DestinationAccountID.String(),
			"timestamp":            t.timeProvider.Now().Format(time.RFC3339),
		}),
		Status:          constants.TransactionStatusPENDING,
		TransactionType: constants.TransactionTypeOUTBOUND,
	}
	pendingOutGoingTransaction, err := t.findOrCreateTransaction(ctx, pendingOutgoingTransactionParams)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), "error while creating pending outgoing trx", nil)
	}
	return pendingOutGoingTransaction, nil
}

func (t *TransactionOperations) createPendingFeeTransaction(ctx context.Context, params TransferParams, sourceAccount accounts.Account) (*transactions.Transaction, error) {
	if params.FeeAmount == nil {
		return nil, nil
	}
	pendingOutgoingFeeTransactionParams := &transactions.Transaction{
		UserID:       &sourceAccount.UserID,
		Amount:       *params.FeeAmount,
		AccountID:    sourceAccount.ID,
		CurrencyCode: constants.CurrencyCode(sourceAccount.Currency),
		ReferenceID:  params.FeeTransactionReferenceID,
		Metadata: datatypes.JSONMap(map[string]interface{}{
			"OperationType":       "Fee Transfer",
			"LinkedTransactionID": params.FeeTransactionReferenceID.String(),
			"LinkedAccountID":     params.SourceAccountID.String(),
			"timestamp":           t.timeProvider.Now().Format(time.RFC3339),
		}),
		Status:          constants.TransactionStatusPENDING,
		TransactionType: constants.TransactionTypeOUTGOINGFEE,
	}
	pendingOutGoingFeeTransaction, err := t.findOrCreateTransaction(ctx, pendingOutgoingFeeTransactionParams)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), "error while creating pending outgoing fee trx", nil)
	}
	return pendingOutGoingFeeTransaction, nil
}

func (t *TransactionOperations) createPendingIncomingTransaction(ctx context.Context, params TransferParams, destinationAccount accounts.Account) (*transactions.Transaction, error) {
	pendingIncomingTransactionParams := &transactions.Transaction{
		UserID:       &destinationAccount.UserID,
		Amount:       params.Amount,
		AccountID:    destinationAccount.ID,
		CurrencyCode: constants.CurrencyCode(destinationAccount.Currency),
		Metadata: datatypes.JSONMap(map[string]interface{}{
			"OperationType":        "Transfer",
			"LinkedTransactionID":  params.DestinationTransactionReferenceID.String(),
			"LinkedAccountID":      params.DestinationAccountID.String(),
			"DestinationAccountID": params.DestinationAccountID.String(),
			"SourceAccountID":      params.SourceAccountID,
			"timestamp":            t.timeProvider.Now().Format(time.RFC3339),
		}),
		ReferenceID: params.DestinationTransactionReferenceID,

		Status:          constants.TransactionStatusPENDING,
		TransactionType: constants.TransactionTypeINBOUND,
	}
	pendingIncomingTransaction, err := t.findOrCreateTransaction(ctx, pendingIncomingTransactionParams)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(err.Error(), "error while creating pending incoming trx", nil)
	}
	return pendingIncomingTransaction, nil
}

func (t *TransactionOperations) updateAccountBalances(ctx context.Context, sourceAcc, destinationAcc accounts.Account, params TransferParams) error {
	sourceAccBalanceUpdateErr := t.accountsService.UpdateBalance(ctx, sourceAcc.ID, params.Amount, constants.BalanceOperationDECREASE.String())
	if sourceAccBalanceUpdateErr != nil {
		return sourceAccBalanceUpdateErr
	}

	destinationAccBalanceUpdateErr := t.accountsService.UpdateBalance(ctx, destinationAcc.ID, params.Amount, constants.BalanceOperationINCREASE.String())
	if destinationAccBalanceUpdateErr != nil {
		return destinationAccBalanceUpdateErr
	}

	return nil
}

func (t *TransactionOperations) finalizeTransactions(ctx context.Context, outgoing, incoming, fee *transactions.Transaction) (*UpdatedTransactions, error) {
	updatedOutgoingTrx, updatedOutgoingTrxErr := t.transactionService.UpdateTransactionStatus(ctx, outgoing.ID, constants.TransactionStatusSUCCESS)
	if updatedOutgoingTrxErr != nil {
		return nil, updatedOutgoingTrxErr
	}

	updatedIncomingTrx, updatedIncomingTrxErr := t.transactionService.UpdateTransactionStatus(ctx, incoming.ID, constants.TransactionStatusSUCCESS)
	if updatedIncomingTrxErr != nil {
		return nil, updatedIncomingTrxErr
	}
	var feeTrx *transactions.Transaction

	if fee != nil {
		fee.Status = constants.TransactionStatusSUCCESS
		updatedFeeTrx, updatedFeeTrxErr := t.transactionService.UpdateTransactionStatus(ctx, fee.ID, constants.TransactionStatusSUCCESS)
		if updatedFeeTrxErr != nil {
			return nil, updatedFeeTrxErr
		}
		feeTrx = updatedFeeTrx
	}

	return &UpdatedTransactions{
		IncomingTrx: *updatedIncomingTrx,
		OutgoingTrx: *updatedOutgoingTrx,
		FeeTrx:      feeTrx,
	}, nil

}

func (t *TransactionOperations) createTransferResult(params TransferParams, outgoing, incoming, fee *transactions.Transaction) *TransferResult {
	return &TransferResult{
		SourceTransactionReferenceID:      params.SourceTransactionReferenceID,
		DestinationTransactionReferenceID: params.DestinationTransactionReferenceID,
		FeeTransactionReferenceID:         params.FeeTransactionReferenceID,
		FeeTransaction:                    fee,
		SourceTransaction:                 outgoing,
		DestinationTransaction:            incoming,
	}
}

func (t *TransactionOperations) findOrCreateTransaction(ctx context.Context, params *transactions.Transaction) (*transactions.Transaction, error) {
	transaction, transactionErr := t.finderOrCreatorService.Call(ctx, params)
	if transactionErr != nil {
		return nil, transactionErr
	}

	return transaction, nil
}

func (t *TransactionOperations) validateAccount(ctx context.Context, transferAmount int, feeAmount *int, sourceAccountID uuid.UUID, destinationAccountID uuid.UUID) (*ValidAccounts, error) {
	sourceAccount, accountErr := t.accountsService.GetAccountByID(ctx, sourceAccountID)
	if accountErr != nil {
		return nil, accountErr
	}

	if sourceAccount == nil {
		return nil, fmt.Errorf("account not found: %s", sourceAccountID)
	}

	if sourceAccount.Status != constants.AccountStatusACTIVE {
		return nil, fmt.Errorf("account is not active: %s", sourceAccount.ID)
	}

	destinationAccount, destinationAccountErr := t.accountsService.GetAccountByID(ctx, destinationAccountID)
	if destinationAccountErr != nil {
		return nil, destinationAccountErr
	}

	if destinationAccount == nil {
		return nil, fmt.Errorf("account not found: %s", destinationAccountID)
	}

	if destinationAccount.Status != constants.AccountStatusACTIVE {
		return nil, fmt.Errorf("account is not active: %s", destinationAccount.ID)
	}

	totalAmount := transferAmount
	if feeAmount != nil {
		totalAmount += *feeAmount
	}

	if totalAmount > sourceAccount.Balance {
		return nil, fmt.Errorf("insufficient balance! transfer amount: %d,  account balance: %d", totalAmount, sourceAccount.Balance)
	}

	return &ValidAccounts{
		SourceAccount:      sourceAccount,
		DestinationAccount: destinationAccount,
	}, nil
}

type ValidAccounts struct {
	SourceAccount      *accounts.Account
	DestinationAccount *accounts.Account
}

type UpdatedTransactions struct {
	IncomingTrx transactions.Transaction
	OutgoingTrx transactions.Transaction
	FeeTrx      *transactions.Transaction
}

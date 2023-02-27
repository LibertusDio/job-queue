package middlewares

import (
	"context"

	jobqueue "github.com/LibertusDio/job-queue"

	"gorm.io/gorm"
)

func GormTransactionMiddleware(contextgormkey string, session *gorm.DB) jobqueue.MiddlewareFunc {
	return func(next jobqueue.HandlerFunc) jobqueue.HandlerFunc {
		return func(ctx context.Context) error {
			tx := session.Begin()
			c := context.WithValue(ctx, contextgormkey, tx)
			err := next(c)
			if err != nil {
				tx.Rollback()
				return err
			}
			return tx.Commit().Error
		}
	}
}

func SelfReferMiddleware(contextqueuekey string, queue jobqueue.Foreman) jobqueue.MiddlewareFunc {
	return func(next jobqueue.HandlerFunc) jobqueue.HandlerFunc {
		return func(ctx context.Context) error {
			c := context.WithValue(ctx, contextqueuekey, queue)
			return next(c)
		}
	}
}

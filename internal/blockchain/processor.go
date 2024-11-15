package blockchain

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"cosmos-evm-exporter/internal/config"
	httpClient "cosmos-evm-exporter/internal/http"
	"cosmos-evm-exporter/internal/logger"
	"cosmos-evm-exporter/internal/metrics"
	"cosmos-evm-exporter/internal/rpc"
)

func NewBlockProcessor(config *config.Config, metrics *metrics.BlockMetrics, logger *logger.Logger) (*BlockProcessor, error) {
	client, err := rpc.NewClient(config.ETHEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create RPC client: %w", err)
	}

	return &BlockProcessor{
		config:  config,
		logger:  logger,
		metrics: metrics,
		client:  client,
	}, nil
}

func (p *BlockProcessor) ProcessBlock(block *BlockResponse) error {
	if block == nil {
		return fmt.Errorf("block is nil")
	}

	header := block.Result.Block.Header

	p.logger.WriteJSONLog("debug", "Processing block", map[string]interface{}{
		"height":           header.Height,
		"proposer_address": header.ProposerAddress,
	}, nil)

	if header.ProposerAddress == "" {
		return fmt.Errorf("received empty proposer address for height %s", header.Height)
	}

	if header.ProposerAddress != p.config.TargetValidator {
		return nil // Not our validator
	}

	clHeight, err := strconv.ParseInt(header.Height, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse block height: %w", err)
	}

	p.metrics.CurrentHeight.Set(float64(clHeight))
	p.metrics.TotalProposed.Inc()
	p.logger.WriteJSONLog("info", "Found validator block", map[string]interface{}{
		"height":           clHeight,
		"proposer_address": header.ProposerAddress,
	}, nil)

	gap, err := p.GetCurrentGap()
	if err != nil {
		return fmt.Errorf("failed to get current gap: %w", err)
	}

	expectedELHeight := clHeight - gap
	return p.checkExecutionBlocks(clHeight, expectedELHeight)
}

func (p *BlockProcessor) checkExecutionBlocks(clHeight, expectedELHeight int64) error {
	startHeight := expectedELHeight - 2
	endHeight := expectedELHeight + 2

	// Check if consensus block was empty
	block, err := GetBlock(httpClient.NewClient(), p.config.RPCEndpoint, clHeight)
	if err == nil && len(block.Result.Block.Data.Txs) == 0 {
		p.metrics.EmptyConsensusBlocks.Inc()
		p.logger.WriteJSONLog("info", "Empty consensus block", map[string]interface{}{
			"height": clHeight,
		}, nil)
	}

	foundBlock := false
	for height := startHeight; height <= endHeight; height++ {
		block, err := p.client.BlockByNumber(context.Background(), big.NewInt(height))
		if err != nil {
			p.logger.WriteJSONLog("error", "Failed to fetch block", map[string]interface{}{
				"height": height,
			}, err)
			continue
		}

		if block.Coinbase().Hex() == p.config.EVMAddress {
			foundBlock = true
			p.metrics.ExecutionConfirmed.Inc()
			p.logger.WriteJSONLog("success", "Found execution block", map[string]interface{}{
				"cl_height": clHeight,
				"el_height": height,
				"hash":      block.Hash().Hex(),
			}, nil)

			if len(block.Transactions()) == 0 {
				p.metrics.EmptyExecutionBlocks.Inc()
				p.logger.WriteJSONLog("info", "Empty execution block", map[string]interface{}{
					"height": height,
				}, nil)
			}
			break
		}
	}

	if !foundBlock {
		p.metrics.ExecutionMissed.Inc()
		p.logger.WriteJSONLog("warn", "Block not found in range", map[string]interface{}{
			"start_height": startHeight,
			"end_height":   endHeight,
		}, nil)
	}

	return nil
}

func (p *BlockProcessor) Start(ctx context.Context) {
	var currentHeight int64
	var errorBlocks int

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Get current height if we don't have it
			if currentHeight == 0 {
				height, err := p.GetCurrentHeight()
				if err != nil {
					p.logger.WriteJSONLog("error", "Failed to get current height", nil, err)
					time.Sleep(2 * time.Second)
					continue
				}
				currentHeight = height
			}

			// Get block
			block, err := GetBlock(httpClient.NewClient(), p.config.RPCEndpoint, currentHeight)
			if err != nil {
				p.metrics.Errors.Inc()
				p.logger.WriteJSONLog("error", "Failed to get block", map[string]interface{}{
					"height": currentHeight,
				}, err)
				errorBlocks++
				time.Sleep(2 * time.Second)
				continue // Don't increment currentHeight on error
			}

			// Process the block
			if err := p.ProcessBlock(block); err != nil {
				p.logger.WriteJSONLog("error", "Error processing block", map[string]interface{}{
					"height": currentHeight,
				}, err)
				errorBlocks++
				// Only increment height if it's not our validator's block
				// This ensures we don't miss our validator's blocks due to temporary errors
				if err.Error() != "block is nil" && block.Result.Block.Header.ProposerAddress != p.config.TargetValidator {
					currentHeight++
				}
				continue
			}

			currentHeight++
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// StartMetricsUpdater starts goroutines that continuously update metrics
func (p *BlockProcessor) StartMetricsUpdater(interval time.Duration) {
	// Update current height metric
	go func() {
		for {
			height, err := p.GetCurrentHeight()
			if err != nil {
				p.logger.WriteJSONLog("error", "Failed to get current height", nil, err)
				p.metrics.Errors.Inc()
			} else {
				p.metrics.CurrentHeight.Set(float64(height))
			}
			time.Sleep(interval)
		}
	}()

	// Update gap metric
	go func() {
		for {
			gap, err := p.GetCurrentGap()
			if err != nil {
				p.logger.WriteJSONLog("error", "Failed to get current gap", nil, err)
				p.metrics.Errors.Inc()
			} else {
				p.metrics.ElToClGap.Set(float64(gap))
			}
			time.Sleep(interval)
		}
	}()
}
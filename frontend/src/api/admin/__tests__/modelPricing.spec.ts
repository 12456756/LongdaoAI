import { describe, expect, it } from 'vitest'
import { candidateAmount, candidateUSD, changePriceCurrency, convertPriceAmount, formatPriceAmount, newModelPrice, pricingUnit } from '../modelPricing'

describe('model quotation currency conversion', () => {
  it('normalizes yuan after applying discount and company markup', () => {
    const price = newModelPrice('yuan')
    changePriceCurrency(price, 'CNY')
    price.items[0].amount = '1.43'; price.discount = '0.5'; price.markup = '2'
    expect(candidateAmount(price.items[0], price)).toBe('1.43')
    expect(candidateUSD(price.items[0], price, '7.15')).toBe('0.2')
    expect(convertPriceAmount('0.2', 'USD', 'CNY', '7.15')).toBe('1.43')
    expect(price.items[0].amount).toBe('1.43')
  })

  it('keeps legacy dollar amounts unchanged and never converts twice', () => {
    const price = newModelPrice('legacy')
    delete price.currency
    price.items[0].amount = '0.1'
    expect(candidateUSD(price.items[0], price, '7.15')).toBe('0.1')
    expect(candidateUSD(price.items[0], price, '8')).toBe('0.1')
    expect(convertPriceAmount('0.1', 'USD', 'USD')).toBe('0.1')
  })

  it('handles free, tiny, repeating and invalid rates with decimal precision', () => {
    expect(convertPriceAmount('0', 'CNY', 'USD', '7.15')).toBe('0')
    expect(convertPriceAmount('0.000000000001', 'CNY', 'USD', '8')).toBe('0.000000000000125')
    expect(formatPriceAmount('0.000000000000125')).toBe('0.000000000000125')
    expect(convertPriceAmount('1', 'CNY', 'USD', '3')).toBe('0.' + '3'.repeat(36))
    expect(convertPriceAmount('2', 'CNY', 'USD', '3')).toBe('0.' + '6'.repeat(35) + '7')
    for (const rate of [undefined, '', '0', '-1', 'invalid']) {
      expect(convertPriceAmount('1', 'CNY', 'USD', rate)).toBe('-')
    }
  })

  it('updates currency units for every billing mode without changing entered numbers', () => {
    for (const mode of ['token', 'image', 'video', 'per_request'] as const) {
      const price = newModelPrice('mode')
      price.mode = mode
      price.items = [{ kind: mode, tier: '', amount: '7.15', unit: pricingUnit(mode) }]
      changePriceCurrency(price, 'CNY')
      expect(price.items[0]).toMatchObject({ amount: '7.15', unit: pricingUnit(mode, 'CNY') })
      expect(candidateUSD(price.items[0], price, '7.15')).toBe('1')
    }
  })
})

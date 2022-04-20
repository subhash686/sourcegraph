import expect from 'expect'
import { test } from 'mocha'

import { Driver, createDriverForTest } from '@sourcegraph/shared/src/testing/driver'

import { PageRoutes } from '../routes.constants'

describe('GlobalNavbar', () => {
    describe('Code Search Dropdown', () => {
        let driver: Driver

        before(async () => {
            driver = await createDriverForTest()
        })

        after(() => driver?.close())

        test('is highlighted on search page', async () => {
            await driver.page.goto(driver.sourcegraphBaseUrl + '/search?q=test&patternType=regexp')

            const active = await driver.page.evaluate(() =>
                document.querySelector(`[data-test-id="${PageRoutes.Search}"]`)?.getAttribute('data-test-active')
            )

            expect(active).toEqual('true')
        })

        test('is highlighted on repo page', async () => {
            await driver.page.goto(driver.sourcegraphBaseUrl + '/github.com/sourcegraph-testing/zap')

            const active = await driver.page.evaluate(() =>
                document.querySelector(`[data-test-id="${PageRoutes.Search}"]`)?.getAttribute('data-test-active')
            )

            expect(active).toEqual('true')
        })

        test('is highlighted on repo file page', async () => {
            await driver.page.goto(driver.sourcegraphBaseUrl + '/github.com/sourcegraph-testing/zap/-/blob/README.md')

            const active = await driver.page.evaluate(() =>
                document.querySelector(`[data-test-id="${PageRoutes.Search}"]`)?.getAttribute('data-test-active')
            )

            expect(active).toEqual('true')
        })

        test('is not highlighted on batch changes page', async () => {
            await driver.page.goto(driver.sourcegraphBaseUrl + '/batch-changes')

            const active = await driver.page.evaluate(() =>
                document.querySelector(`[data-test-id="${PageRoutes.Search}"]`)?.getAttribute('data-test-active')
            )

            expect(active).toEqual('false')
        })
    })
})

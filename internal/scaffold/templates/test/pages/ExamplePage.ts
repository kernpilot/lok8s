import type { Page } from '@playwright/test'
import { config } from '../utils/config'
import { BasePage } from './BasePage'

/**
 * Worked-example page object for the primary web app (`config.urls.app`).
 *
 * This is a TEMPLATE — copy it per service you test (e.g. an AuthPage for your
 * IdP login portal, a WebsitePage for the marketing site). Selectors prefer
 * roles/labels/test-ids so they survive markup churn. Routes live as static
 * data so specs reference `ExamplePage.routes.*` instead of literals.
 */
export class ExamplePage extends BasePage {
  protected readonly serviceUrl = config.urls.app

  constructor(page: Page) {
    super(page)
  }

  /** Known routes — edit for your app. */
  static readonly routes = {
    home: '/',
    login: '/login',
  }

  async gotoHome() {
    return this.goto(ExamplePage.routes.home)
  }

  heading() {
    return this.page.getByRole('heading').first()
  }
}

import { app, getNextAssetDefinitionID, mockEventStreamWebSocket } from '../../../common';
import { testContent } from '../../../samples';
import nock from 'nock';
import request from 'supertest';
import assert from 'assert';
import { IDBAssetDefinition, IEventAssetDefinitionCreated } from '../../../../lib/interfaces';
import * as utils from '../../../../lib/utils';

let publicAssetDefinitionID = getNextAssetDefinitionID();

describe('Assets: authored - public - structured', async () => {

  describe('Create asset definition', () => {

    const timestamp = utils.getTimestamp();

    it('Checks that the asset definition can be added', async () => {

      nock('https://apigateway.kaleido.io')
        .post('/createStructuredAssetDefinition?kld-from=0x0000000000000000000000000000000000000001&kld-sync=true')
        .reply(200);

      nock('https://ipfs.kaleido.io')
        .post('/api/v0/add')
        .reply(200, { Hash: 'QmV85fRf9jng5zhcSC4Zef2dy8ypouazgckRz4GhA5cUgw' });

      const result = await request(app)
        .post('/api/v1/assets/definitions')
        .send({
          name: 'authored - public - structured',
          author: '0x0000000000000000000000000000000000000001',
          isContentPrivate: false,
          contentSchema: testContent.schema.object
        })
        .expect(200);
      assert.deepStrictEqual(result.body, { status: 'submitted' });

      const getAssetDefinitionsResponse = await request(app)
        .get('/api/v1/assets/definitions')
        .expect(200);
      const assetDefinition = getAssetDefinitionsResponse.body.find((assetDefinition: IDBAssetDefinition) => assetDefinition.name === 'authored - public - structured');
      assert.strictEqual(assetDefinition.author, '0x0000000000000000000000000000000000000001');
      assert.strictEqual(assetDefinition.confirmed, false);
      assert.strictEqual(assetDefinition.isContentPrivate, false);
      assert.deepStrictEqual(assetDefinition.contentSchema, testContent.schema.object);
      assert.strictEqual(assetDefinition.name, 'authored - public - structured');
      assert.strictEqual(typeof assetDefinition.timestamp, 'number');
    });

    it('Checks that the event stream notification for confirming the asset definition creation is handled', async () => {
      const eventPromise = new Promise((resolve) => {
        mockEventStreamWebSocket.once('send', message => {
          assert.strictEqual(message, '{"type":"ack","topic":"dev"}');
          resolve();
        })
      });
      const data: IEventAssetDefinitionCreated = {
        assetDefinitionID: publicAssetDefinitionID.toString(),
        author: '0x0000000000000000000000000000000000000001',
        name: 'authored - public - structured',
        contentSchemaHash: testContent.schema.ipfsSha256,
        isContentPrivate: false,
        timestamp: timestamp.toString()
      };
      mockEventStreamWebSocket.emit('message', JSON.stringify([{
        signature: utils.contractEventSignatures.STRUCTURED_ASSET_DEFINITION_CREATED,
        data
      }]));
      await eventPromise;
    });

    it('Checks that the asset definition is confirmed', async () => {
      const getAssetDefinitionsResponse = await request(app)
        .get('/api/v1/assets/definitions')
        .expect(200);
      const assetDefinition = getAssetDefinitionsResponse.body.find((assetDefinition: IDBAssetDefinition) => assetDefinition.name === 'authored - public - structured');
      assert.strictEqual(assetDefinition.assetDefinitionID, publicAssetDefinitionID);
      assert.strictEqual(assetDefinition.author, '0x0000000000000000000000000000000000000001');
      assert.strictEqual(assetDefinition.confirmed, true);
      assert.strictEqual(assetDefinition.isContentPrivate, false);
      assert.deepStrictEqual(assetDefinition.contentSchema, testContent.schema.object);
      assert.strictEqual(assetDefinition.name, 'authored - public - structured');
      assert.strictEqual(assetDefinition.timestamp, timestamp);

      const getAssetDefinitionResponse = await request(app)
      .get(`/api/v1/assets/definitions/${publicAssetDefinitionID}`)
      .expect(200);
      assert.deepStrictEqual(assetDefinition, getAssetDefinitionResponse.body);
    });

  });

});
